package mongofiles

type StorageOptions struct {
	// Specified database to use. defaults to 'test' if none is specified
	DB string `short:"d" default:"test" default-mask:"-" long:"db" description:"database to use (default is 'test')"`

	// 'LocalFileName' is an option that specifies what filename to use for (put|get)
	LocalFileName string `long:"local" short:"l" description:"local filename for put|get"`

	// 'ContentType' is an option that specifies the Content/MIME type to use for 'put'
	ContentType string `long:"type" short:"t" description:"content/MIME type for put (optional)"`

	// if set, 'Replace' will remove other files with same name after 'put'
	Replace bool `long:"replace" short:"r" description:"remove other files with same name after put"`

	// GridFSPrefix specifies what GridFS prefix to use; defaults to 'fs'
	GridFSPrefix string `long:"prefix" default:"fs" default-mask:"-" description:"GridFS prefix to use (default is 'fs')"`

	// Specifies the write concern for each write operation that mongofiles writes to the target database.
	// By default, mongofiles waits for a majority of members from the replica set to respond before returning.
	WriteConcern string `long:"writeConcern" default:"majority" default-mask:"-" description:"write concern options e.g. --writeConcern majority, --writeConcern '{w: 3, wtimeout: 500, fsync: true, j: true}' (defaults to 'majority')"`
}

func (_ *StorageOptions) Name() string {
	return "storage"
}
