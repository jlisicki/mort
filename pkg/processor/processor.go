package processor

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/aldor007/mort/pkg/cache"

	"github.com/aldor007/mort/pkg/config"
	"github.com/aldor007/mort/pkg/engine"
	"github.com/aldor007/mort/pkg/lock"
	"github.com/aldor007/mort/pkg/middleware"
	"github.com/aldor007/mort/pkg/monitoring"
	"github.com/aldor007/mort/pkg/object"
	"github.com/aldor007/mort/pkg/processor/plugins"
	"github.com/aldor007/mort/pkg/response"
	"github.com/aldor007/mort/pkg/storage"
	"github.com/aldor007/mort/pkg/throttler"
	"github.com/aldor007/mort/pkg/transforms"
	"go.uber.org/zap"
)

const s3LocationStr = "<?xml version=\"1.0\" encoding=\"UTF-8\"?><LocationConstraint xmlns=\"http://s3.amazonaws.com/doc/2006-03-01/\">EU</LocationConstraint>"

var (
	errTimeout       = errors.New("timeout")         // error when timeout
	errContextCancel = errors.New("context timeout") // error when context timeout
	errThrottled     = errors.New("throttled")       // error when request throttled
)

// NewRequestProcessor create instance of request processor
// It main component of mort it handle all of requests
func NewRequestProcessor(serverConfig config.Server, l lock.Lock, throttler throttler.Throttler) RequestProcessor {
	rp := RequestProcessor{}
	rp.collapse = l
	rp.throttler = throttler
	rp.processTimeout = time.Duration(serverConfig.RequestTimeout) * time.Second
	rp.lockTimeout = time.Duration(serverConfig.LockTimeout) * time.Second
	rp.serverConfig = serverConfig
	rp.plugins = plugins.NewPluginsManager(serverConfig.Plugins)
	rp.responseCache = cache.Create(serverConfig.Cache)
	return rp
}

// RequestProcessor handle incoming requests
type RequestProcessor struct {
	collapse       lock.Lock              // interface used for request collapsing
	throttler      throttler.Throttler    // interface used for rate limiting creating of new images
	processTimeout time.Duration          // request processing timeout
	lockTimeout    time.Duration          // lock timeout for collapsed request it equal processTimeout - 1 s
	plugins        plugins.PluginsManager // plugins run plugins before some phases of requests processing
	serverConfig   config.Server
	responseCache  cache.ResponseCache
}

type requestMessage struct {
	responseChan chan *response.Response
	obj          *object.FileObject
	request      *http.Request
	cancel       chan struct{}
}

// Process handle incoming request and create response
func (r *RequestProcessor) Process(req *http.Request, obj *object.FileObject) *response.Response {
	pCtx := req.Context()
	ctx, timeout := context.WithTimeout(pCtx, r.processTimeout)
	obj.FillWithRequest(req, ctx)
	defer timeout()
	r.plugins.PreProcess(obj, req)
	msg := requestMessage{}
	msg.request = req
	msg.obj = obj
	msg.responseChan = make(chan *response.Response)
	msg.cancel = make(chan struct{}, 1)

	go r.processChan(ctx, msg)

	select {
	case <-ctx.Done():
		close(msg.cancel)
		monitoring.Log().Warn("Process timeout", obj.LogData(zap.String("error", "Context.timeout"))...)
		return r.replyWithError(obj, 499, errContextCancel)
	case res := <-msg.responseChan:
		r.plugins.PostProcess(obj, req, res)
		return res
	}

}

func (r *RequestProcessor) processChan(ctx context.Context, msg requestMessage) {
	res := r.process(msg.request, msg.obj)
	select {
	case <-msg.cancel:
		res.Close()
		return
	case <-ctx.Done():
		res.Close()
		return
	case msg.responseChan <- res:
		return
	}
}

func (r *RequestProcessor) replyWithError(obj *object.FileObject, sc int, err error) *response.Response {
	if !obj.HasTransform() || obj.Debug || r.serverConfig.PlaceholderStr == "" {
		return response.NewError(sc, err)
	}

	errorObject, errCreate := object.NewFileErrorObject(r.serverConfig.PlaceholderStr, obj)
	if errCreate != nil {
		return response.NewError(sc, err)
	}

	if cacheRes, errCache := r.responseCache.Get(errorObject); errCache == nil {
		cacheRes.StatusCode = sc
		return cacheRes
	}

	go func() {
		lockData, locked := r.collapse.Lock(errorObject.Key)
		if locked {
			defer r.collapse.Release(errorObject.Key)
			monitoring.Log().Info("Lock acquired for error response", obj.LogData()...)
			parent := response.NewBuf(200, r.serverConfig.Placeholder.Buf)
			transformsTab := []transforms.Transforms{obj.Transforms}

			eng := engine.NewImageEngine(parent)
			res, err := eng.Process(obj, transformsTab)
			if err == nil {
				res.StatusCode = sc
				r.responseCache.Set(errorObject, updateHeaders(errorObject, res))
			}
		} else {
			lockData.Cancel <- true

		}
	}()

	res := response.NewBuf(sc, r.serverConfig.Placeholder.Buf)
	res.SetContentType(r.serverConfig.Placeholder.ContentType)
	return res
}

func (r *RequestProcessor) process(req *http.Request, obj *object.FileObject) *response.Response {

	switch req.Method {
	case "GET", "HEAD":
		if obj.Key == "" {
			return handleS3Get(req, obj)
		}

		// todo Cache layer should be protected by memory lock.
		res, err := r.responseCache.Get(obj)
		if err == nil {
			return res
		}

		if obj.HasTransform() {
			res = updateHeaders(obj, r.collapseGET(req, obj))
		} else {
			res = updateHeaders(obj, r.handleGET(req, obj))
		}

		if res.IsCacheable() && res.ContentLength != -1 && res.ContentLength < r.serverConfig.Cache.MaxCacheItemSize {
			resCpy, err := res.Copy()
			objCpy := obj.Copy()
			if err == nil {
				go func() {
					resCpy.Body()
					err = r.responseCache.Set(objCpy, resCpy)
					if err != nil {
						monitoring.Log().Error("response cache error set", obj.LogData(zap.Error(err))...)
					}
				}()
			}
		}

		return res
	case "PUT":
		go r.responseCache.Delete(obj)
		return handlePUT(req, obj)
	case "DELETE":
		go r.responseCache.Delete(obj)
		return storage.Delete(obj)

	default:
		return response.NewError(405, errors.New("method not allowed"))
	}

}

func handlePUT(req *http.Request, obj *object.FileObject) *response.Response {
	defer req.Body.Close()
	return storage.Set(obj, req.Header, req.ContentLength, req.Body)
}

func (r *RequestProcessor) collapseGET(req *http.Request, obj *object.FileObject) *response.Response {
	ctx := obj.Ctx
	lockResult, locked := r.collapse.Lock(obj.Key)
	if locked {
		monitoring.Log().Info("Lock acquired", obj.LogData()...)
		res := r.handleGET(req, obj)
		r.collapse.NotifyAndRelease(obj.Key, res)
		return res
	}

	monitoring.Report().Inc("collapsed_count")
	monitoring.Log().Info("Lock not acquired", obj.LogData()...)
	timer := time.NewTimer(r.lockTimeout)

	for {

		select {
		case <-ctx.Done():
			lockResult.Cancel <- true
			return r.replyWithError(obj, 504, errContextCancel)
		case res, ok := <-lockResult.ResponseChan:
			if !ok {
				return r.handleGET(req, obj)
			}
			return res
		case <-timer.C:
			lockResult.Cancel <- true
			if cacheRes, err := r.responseCache.Get(obj); err == nil {
				return cacheRes
			}
			return r.replyWithError(obj, 504, errTimeout)
		}
	}

}

// Handle single GET
// nolint: gocyclo
func (r *RequestProcessor) handleGET(req *http.Request, obj *object.FileObject) *response.Response {
	ctx := obj.Ctx

	currObj := obj
	var parentObj *object.FileObject
	var transformsTab []transforms.Transforms
	var res *response.Response
	var parentRes *response.Response

	// search for root parent
	for currObj.HasParent() {
		if currObj.HasTransform() {
			transformsTab = append(transformsTab, currObj.Transforms)
		}
		currObj = currObj.Parent

		if !currObj.HasParent() {
			parentObj = currObj
		}
	}

	resChan := make(chan *response.Response, 1)
	parentChan := make(chan *response.Response, 1)

	go func(o *object.FileObject) {
		resp := storage.Get(o)
		// Ensure before passing the response that the context is not canceled.
		// In such case Close the response.
		// Passing the data to respChan and checking ctx.Done cannot be
		// done in a single select since golang randomly choice which channel
		// to handle. So when ctx.Done is closed there is 50% of chance that
		// it will choice to send resp to resChan.
		select {
		case <-ctx.Done():
			resp.Close()
			return
		default:
		}
		select {
		case resChan <- resp:
			return
		default:
		}
	}(obj)

	// get parent from storage
	if parentObj != nil && obj.CheckParent {
		go func(p *object.FileObject) {
			select {
			case <-ctx.Done():
				return
			case parentChan <- storage.Head(p):
				return
			}
		}(parentObj)
	}

	for {
		select {
		case <-ctx.Done():
			return r.replyWithError(obj, 499, errContextCancel)
		case res = <-resChan:
			if obj.CheckParent && parentObj != nil && (parentRes == nil || parentRes.StatusCode == 0) {
				go func() {
					resChan <- res
				}()

			} else {
				if res.StatusCode == 404 {
					res = r.handleNotFound(obj, parentObj, transformsTab, parentRes, res)
					select {
					case <-ctx.Done():
						res.Close()
						return res
					default:
						return res
					}
				}

				monitoring.Report().Inc("request_type;type:download")

				if res.StatusCode > 199 && res.StatusCode < 299 {
					if obj.CheckParent && parentObj != nil && parentRes.StatusCode == 200 {
						return res
					}

					return res
				}

				return res
			}
		case parentRes = <-parentChan:
			if parentRes.StatusCode == 404 {
				return parentRes
			}
		}
	}

}

func (r *RequestProcessor) handleNotFound(obj, parentObj *object.FileObject, transformsTab []transforms.Transforms, parentRes, res *response.Response) *response.Response {
	// We can close res as we will not use it
	res.Close()
	if parentObj == nil {
		return res
	}

	if !obj.CheckParent {
		parentRes = storage.Head(parentObj)
	}

	if parentRes.HasError() {
		return r.replyWithError(obj, parentRes.StatusCode, parentRes.Error())
	} else if parentRes.StatusCode == 404 {
		monitoring.Log().Warn("Missing parent for object", obj.LogData()...)
		return parentRes
	}
	parentRes.Close()
	if parentRes.StatusCode != 200 || !parentRes.IsImage() {
		// monitoring.Log().Warn("Not performing transforms", obj.LogData(zap.Int("parent.sc", parentRes.StatusCode),
		// 	zap.String("parent.ContentType", parentRes.Headers.Get(response.HeaderContentType)), zap.Error(parentRes.Error()))...)
		return res
	}
	parentRes = storage.Get(parentObj)
	if obj.HasTransform() {
		// processImage returns new response so both parentRes must be closed
		defer parentRes.Close()
		return r.processImage(obj, parentRes, transformsTab)
	}
	return parentRes
}

func handleS3Get(req *http.Request, obj *object.FileObject) *response.Response {
	query := req.URL.Query()

	if _, ok := query["location"]; ok {
		return response.NewString(200, s3LocationStr)
	}

	maxKeys := 1000
	delimeter := ""
	prefix := ""
	marker := ""

	if maxKeysQuery, ok := query["max-keys"]; ok {
		maxKeys, _ = strconv.Atoi(maxKeysQuery[0])
	}

	if delimeterQuery, ok := query["delimeter"]; ok {
		delimeter = delimeterQuery[0]
	}

	if prefixQuery, ok := query["prefix"]; ok {
		prefix = prefixQuery[0]
	}

	if markerQuery, ok := query["marker"]; ok {
		marker = markerQuery[0]
	}

	return storage.List(obj, maxKeys, delimeter, prefix, marker)

}

func (r *RequestProcessor) processImage(obj *object.FileObject, parent *response.Response, transformsTab []transforms.Transforms) *response.Response {
	monitoring.Report().Inc("request_type;type:transform")
	ctx := obj.Ctx
	taked := r.throttler.Take(ctx)
	if !taked {
		monitoring.Log().Warn("Processor/processImage", obj.LogData(zap.String("error", "throttled"))...)
		monitoring.Report().Inc("throttled_count")
		return r.replyWithError(obj, 503, errThrottled)
	}
	defer r.throttler.Release()

	transformsLen := len(transformsTab)
	mergedTrans := transforms.Merge(transformsTab)
	mergedLen := len(mergedTrans)

	monitoring.Log().Info("Performing transforms", obj.LogData(zap.Int("transformsLen", transformsLen), zap.Int("mergedLen", mergedLen))...)
	eng := engine.NewImageEngine(parent)
	res, err := eng.Process(obj, mergedTrans)
	if err != nil {
		errRes := response.NewError(400, err)
		errRes.SetTransforms(mergedTrans)
		return errRes
	}
	res.SetTransforms(mergedTrans)

	if err := storeProcessedImage(res, obj); err != nil {
		monitoring.Log().Warn("Processor/processImage", obj.LogData(zap.Error(err))...)
	}

	return res
}

func storeProcessedImage(res *response.Response, obj *object.FileObject) error {
	resCpy, err := res.Copy()
	if err != nil {
		return err
	}
	go func(objS object.FileObject, resS *response.Response) {
		storage.Set(&objS, resS.Headers, resS.ContentLength, resS.Stream())
		resS.Close()
	}(*obj, resCpy)
	return nil
}

func updateHeaders(obj *object.FileObject, res *response.Response) *response.Response {
	ctx := obj.Ctx

	mortConfig := config.GetInstance()
	headers := mortConfig.Headers
	bucket, ok := mortConfig.Buckets[obj.Bucket]

	if ok {
		for h, v := range bucket.Headers {
			if res.Headers.Get(h) == "" {
				res.Set(h, v)
			}
		}
	}

	for _, headerPred := range headers {
		for _, status := range headerPred.StatusCodes {
			if status == res.StatusCode {
				for h, v := range headerPred.Values {
					if headerPred.Override {
						res.Set(h, v)
					} else if res.Headers.Get(h) == "" {
						res.Set(h, v)
					}
				}
				return res
			}
		}
	}

	if ctx.Value(middleware.S3AuthCtxKey) != nil {
		res.Set("Cache-Control", "no-cache")
	}

	return res
}
