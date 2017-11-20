package object

import (
	"github.com/aldor007/mort/config"
	"github.com/aldor007/mort/log"
	"github.com/aldor007/mort/transforms"
	"go.uber.org/zap"
	"net/url"
)

// FileObject is representing parsed request for image or file
type FileObject struct {
	Uri         *url.URL              `json:"uri"`        // original request path
	Bucket      string                `json:"bucket"`     // request matched bucket
	Key         string                `json:"key"`        // storage path for file
	Transforms  transforms.Transforms `json:"transforms"` // list of transform that should be performed
	Storage     config.Storage        `json:"storage"`    // selected storage that should be used
	Parent      *FileObject           // original image for transformed image
	CheckParent bool                  // boolen if we should always check if parent exists
}

// NewFileObjectFromPath create new instance of FileObject
// path should be request path
// mortConfig should be pointer to current buckets config
func NewFileObjectFromPath(path string, mortConfig *config.Config) (*FileObject, error) {
	obj := FileObject{}
	obj.Uri = &url.URL{}
	obj.Uri.Path = path

	//obj.uriBytes = []byte(uri)
	obj.CheckParent = false

	err := Parse(obj.Uri, mortConfig, &obj)

	log.Log().Info("FileObject", zap.String("path", path), zap.String("key", obj.Key), zap.String("bucket", obj.Bucket), zap.String("storage", obj.Storage.Kind),
		zap.Bool("hasTransforms", !obj.Transforms.NotEmpty), zap.Bool("hasParent", obj.HasParent()))
	return &obj, err
}

// NewFileObject create new instance of FileObject
// uri is request URL
// mortConfig should be pointer to current buckets config
func NewFileObject(uri *url.URL, mortConfig *config.Config) (*FileObject, error) {
	obj := FileObject{}
	obj.Uri = uri
	//obj.uriBytes = []byte(uri)
	obj.CheckParent = false

	err := Parse(uri, mortConfig, &obj)

	log.Log().Info("FileObject", zap.String("path", uri.Path), zap.String("key", obj.Key), zap.String("bucket", obj.Bucket), zap.String("storage", obj.Storage.Kind),
		zap.Bool("hasTransforms", !obj.Transforms.NotEmpty), zap.Bool("hasParent", obj.HasParent()))
	return &obj, err
}

// HasParent inform if object has parent
func (o *FileObject) HasParent() bool {
	return o.Parent != nil
}

// HasTransform inform if object has transform
func (o *FileObject) HasTransform() bool {
	return o.Transforms.NotEmpty == true
}
