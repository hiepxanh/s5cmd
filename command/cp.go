package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/go-multierror"
	"github.com/urfave/cli/v2"

	errorpkg "github.com/peak/s5cmd/v2/error"
	"github.com/peak/s5cmd/v2/log"
	"github.com/peak/s5cmd/v2/log/stat"
	"github.com/peak/s5cmd/v2/parallel"
	"github.com/peak/s5cmd/v2/storage"
	"github.com/peak/s5cmd/v2/storage/url"
)

const (
	defaultCopyConcurrency = 5
	defaultPartSize        = 50 // MiB
	megabytes              = 1024 * 1024
)

var copyHelpTemplate = `Name:
	{{.HelpName}} - {{.Usage}}

Usage:
	{{.HelpName}} [options] source destination

Options:
	{{range .VisibleFlags}}{{.}}
	{{end}}
Examples:
	01. Download an S3 object to working directory
		 > s5cmd {{.HelpName}} s3://bucket/prefix/object.gz .

	02. Download an S3 object and rename
		 > s5cmd {{.HelpName}} s3://bucket/prefix/object.gz myobject.gz

	03. Download all S3 objects to a directory
		 > s5cmd {{.HelpName}} "s3://bucket/*" target-directory/

	04. Download an S3 object from a public bucket
		 > s5cmd --no-sign-request {{.HelpName}} s3://bucket/prefix/object.gz .

	05. Upload a file to S3 bucket
		 > s5cmd {{.HelpName}} myfile.gz s3://bucket/

	06. Upload matching files to S3 bucket
		 > s5cmd {{.HelpName}} "dir/*.gz" s3://bucket/

	07. Upload all files in a directory to S3 bucket recursively
		 > s5cmd {{.HelpName}} dir/ s3://bucket/

	08. Copy S3 object to another bucket
		 > s5cmd {{.HelpName}} s3://bucket/object s3://target-bucket/prefix/object

	09. Copy matching S3 objects to another bucket
		 > s5cmd {{.HelpName}} "s3://bucket/*.gz" s3://target-bucket/prefix/

	10. Copy files in a directory to S3 prefix if not found on target
		 > s5cmd {{.HelpName}} -n -s -u dir/ s3://bucket/target-prefix/

	11. Copy files in an S3 prefix to another S3 prefix if not found on target
		 > s5cmd {{.HelpName}} -n -s -u "s3://bucket/source-prefix/*" s3://bucket/target-prefix/

	12. Perform KMS Server Side Encryption of the object(s) at the destination
		 > s5cmd {{.HelpName}} --sse aws:kms s3://bucket/object s3://target-bucket/prefix/object

	13. Perform KMS-SSE of the object(s) at the destination using customer managed Customer Master Key (CMK) key id
		 > s5cmd {{.HelpName}} --sse aws:kms --sse-kms-key-id <your-kms-key-id> s3://bucket/object s3://target-bucket/prefix/object

	14. Force transfer of GLACIER objects with a prefix whether they are restored or not
		 > s5cmd {{.HelpName}} --force-glacier-transfer "s3://bucket/prefix/*" target-directory/

	15. Upload a file to S3 bucket with public read s3 acl
		 > s5cmd {{.HelpName}} --acl "public-read" myfile.gz s3://bucket/

	16. Upload a file to S3 bucket with expires header
		 > s5cmd {{.HelpName}} --expires "2024-10-01T20:30:00Z" myfile.gz s3://bucket/

	17. Upload a file to S3 bucket with cache-control header
		 > s5cmd {{.HelpName}} --cache-control "public, max-age=345600" myfile.gz s3://bucket/

	18. Copy all files to S3 bucket but exclude the ones with txt and gz extension
		 > s5cmd {{.HelpName}} --exclude "*.txt" --exclude "*.gz" dir/ s3://bucket

	19. Copy all files from S3 bucket to another S3 bucket but exclude the ones starts with log
		 > s5cmd {{.HelpName}} --exclude "log*" "s3://bucket/*" s3://destbucket

	20. Download an S3 object from a requester pays bucket
		 > s5cmd --request-payer=requester {{.HelpName}} s3://bucket/prefix/object.gz .

	21. Upload a file to S3 with a content-type and content-encoding header
		 > s5cmd --content-type "text/css" --content-encoding "br" myfile.css.br s3://bucket/
		 
	22. Download the specific version of a remote object to working directory
		 > s5cmd {{.HelpName}} --version-id VERSION_ID s3://bucket/prefix/object .
`

func NewSharedFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{
			Name:  "no-follow-symlinks",
			Usage: "do not follow symbolic links",
		},
		&cli.StringFlag{
			Name:  "storage-class",
			Usage: "set storage class for target ('STANDARD','REDUCED_REDUNDANCY','GLACIER','STANDARD_IA','ONEZONE_IA','INTELLIGENT_TIERING','DEEP_ARCHIVE')",
		},
		&cli.IntFlag{
			Name:    "concurrency",
			Aliases: []string{"c"},
			Value:   defaultCopyConcurrency,
			Usage:   "number of concurrent parts transferred between host and remote server",
		},
		&cli.IntFlag{
			Name:    "part-size",
			Aliases: []string{"p"},
			Value:   defaultPartSize,
			Usage:   "size of each part transferred between host and remote server, in MiB",
		},
		&cli.StringFlag{
			Name:  "sse",
			Usage: "perform server side encryption of the data at its destination, e.g. aws:kms",
		},
		&cli.StringFlag{
			Name:  "sse-kms-key-id",
			Usage: "customer master key (CMK) id for SSE-KMS encryption; leave it out if server-side generated key is desired",
		},
		&cli.StringFlag{
			Name:  "acl",
			Usage: "set acl for target: defines granted accesses and their types on different accounts/groups, e.g. cp --acl 'public-read'",
		},
		&cli.StringFlag{
			Name:  "cache-control",
			Usage: "set cache control for target: defines cache control header for object, e.g. cp --cache-control 'public, max-age=345600'",
		},
		&cli.StringFlag{
			Name:  "expires",
			Usage: "set expires for target (uses RFC3339 format): defines expires header for object, e.g. cp  --expires '2024-10-01T20:30:00Z'",
		},
		&cli.BoolFlag{
			Name:  "force-glacier-transfer",
			Usage: "force transfer of glacier objects whether they are restored or not",
		},
		&cli.BoolFlag{
			Name:  "ignore-glacier-warnings",
			Usage: "turns off glacier warnings: ignore errors encountered during copying, downloading and moving glacier objects",
		},
		&cli.StringFlag{
			Name:  "source-region",
			Usage: "set the region of source bucket; the region of the source bucket will be automatically discovered if --source-region is not specified",
		},
		&cli.StringFlag{
			Name:  "destination-region",
			Usage: "set the region of destination bucket: the region of the destination bucket will be automatically discovered if --destination-region is not specified",
		},
		&cli.StringSliceFlag{
			Name:  "exclude",
			Usage: "exclude objects with given pattern",
		},
		&cli.BoolFlag{
			Name:  "raw",
			Usage: "disable the wildcard operations, useful with filenames that contains glob characters",
		},
		&cli.StringFlag{
			Name:  "content-type",
			Usage: "set content type for target: defines content type header for object, e.g. --content-type text/plain",
		},
		&cli.StringFlag{
			Name:  "content-encoding",
			Usage: "set content encoding for target: defines content encoding header for object, e.g. --content-encoding gzip",
		},
		&cli.IntFlag{
			Name:        "no-such-upload-retry-count",
			Usage:       "number of times that a request will be retried on NoSuchUpload error; you should not use this unless you really know what you're doing",
			DefaultText: "0",
			Hidden:      true,
		},
	}
}

func NewCopyCommandFlags() []cli.Flag {
	copyFlags := []cli.Flag{
		&cli.BoolFlag{
			Name:    "flatten",
			Aliases: []string{"f"},
			Usage:   "flatten directory structure of source, starting from the first wildcard",
		},
		&cli.BoolFlag{
			Name:    "no-clobber",
			Aliases: []string{"n"},
			Usage:   "do not overwrite destination if already exists",
		},
		&cli.BoolFlag{
			Name:    "if-size-differ",
			Aliases: []string{"s"},
			Usage:   "only overwrite destination if size differs",
		},
		&cli.BoolFlag{
			Name:    "if-source-newer",
			Aliases: []string{"u"},
			Usage:   "only overwrite destination if source modtime is newer",
		},
		&cli.StringFlag{
			Name:  "version-id",
			Usage: "use the specified version of an object",
		},
	}
	sharedFlags := NewSharedFlags()
	return append(copyFlags, sharedFlags...)
}

func NewCopyCommand() *cli.Command {
	cmd := &cli.Command{
		Name:               "cp",
		HelpName:           "cp",
		Usage:              "copy objects",
		Flags:              NewCopyCommandFlags(),
		CustomHelpTemplate: copyHelpTemplate,
		Before: func(c *cli.Context) error {
			err := validateCopyCommand(c)
			if err != nil {
				printError(commandFromContext(c), c.Command.Name, err)
			}
			return err
		},
		Action: func(c *cli.Context) (err error) {
			defer stat.Collect(c.Command.FullName(), &err)()

			// don't delete source
			copy, err := NewCopy(c, false)
			if err != nil {
				return err
			}
			return copy.Run(c.Context)
		},
	}

	cmd.BashComplete = getBashCompleteFn(cmd, false, false)
	return cmd
}

// Copy holds copy operation flags and states.
type Copy struct {
	src         *url.URL
	dst         *url.URL
	op          string
	fullCommand string

	deleteSource bool

	// flags
	noClobber             bool
	ifSizeDiffer          bool
	ifSourceNewer         bool
	flatten               bool
	followSymlinks        bool
	storageClass          storage.StorageClass
	encryptionMethod      string
	encryptionKeyID       string
	acl                   string
	forceGlacierTransfer  bool
	ignoreGlacierWarnings bool
	exclude               []string
	cacheControl          string
	expires               string
	contentType           string
	contentEncoding       string

	// region settings
	srcRegion string
	dstRegion string

	// s3 options
	concurrency int
	partSize    int64
	storageOpts storage.Options
}

// NewCopy creates Copy from cli.Context.
func NewCopy(c *cli.Context, deleteSource bool) (*Copy, error) {
	fullCommand := commandFromContext(c)

	src, err := url.New(c.Args().Get(0), url.WithVersion(c.String("version-id")),
		url.WithRaw(c.Bool("raw")))
	if err != nil {
		printError(fullCommand, c.Command.Name, err)
		return nil, err
	}

	dst, err := url.New(c.Args().Get(1), url.WithRaw(c.Bool("raw")))
	if err != nil {
		printError(fullCommand, c.Command.Name, err)
		return nil, err
	}

	return &Copy{
		src:          src,
		dst:          dst,
		op:           c.Command.Name,
		fullCommand:  fullCommand,
		deleteSource: deleteSource,
		// flags
		noClobber:             c.Bool("no-clobber"),
		ifSizeDiffer:          c.Bool("if-size-differ"),
		ifSourceNewer:         c.Bool("if-source-newer"),
		flatten:               c.Bool("flatten"),
		followSymlinks:        !c.Bool("no-follow-symlinks"),
		storageClass:          storage.StorageClass(c.String("storage-class")),
		concurrency:           c.Int("concurrency"),
		partSize:              c.Int64("part-size") * megabytes,
		encryptionMethod:      c.String("sse"),
		encryptionKeyID:       c.String("sse-kms-key-id"),
		acl:                   c.String("acl"),
		forceGlacierTransfer:  c.Bool("force-glacier-transfer"),
		ignoreGlacierWarnings: c.Bool("ignore-glacier-warnings"),
		exclude:               c.StringSlice("exclude"),
		cacheControl:          c.String("cache-control"),
		expires:               c.String("expires"),
		contentType:           c.String("content-type"),
		contentEncoding:       c.String("content-encoding"),
		// region settings
		srcRegion: c.String("source-region"),
		dstRegion: c.String("destination-region"),

		storageOpts: NewStorageOpts(c),
	}, nil
}

const fdlimitWarning = `
WARNING: s5cmd is hitting the max open file limit allowed by your OS. Either
increase the open file limit or try to decrease the number of workers with
'-numworkers' parameter.
`

// Run starts copying given source objects to destination.
func (c Copy) Run(ctx context.Context) error {
	// override source region if set
	if c.srcRegion != "" {
		c.storageOpts.SetRegion(c.srcRegion)
	}

	client, err := storage.NewClient(ctx, c.src, c.storageOpts)
	if err != nil {
		printError(c.fullCommand, c.op, err)
		return err
	}

	objch, err := expandSource(ctx, client, c.followSymlinks, c.src)

	if err != nil {
		printError(c.fullCommand, c.op, err)
		return err
	}

	waiter := parallel.NewWaiter()

	var (
		merrorWaiter  error
		merrorObjects error
		errDoneCh     = make(chan bool)
	)

	go func() {
		defer close(errDoneCh)
		for err := range waiter.Err() {
			if strings.Contains(err.Error(), "too many open files") {
				fmt.Println(strings.TrimSpace(fdlimitWarning))
				fmt.Printf("ERROR %v\n", err)

				os.Exit(1)
			}
			printError(c.fullCommand, c.op, err)
			merrorWaiter = multierror.Append(merrorWaiter, err)
		}
	}()

	isBatch := c.src.IsWildcard()
	if !isBatch && !c.src.IsRemote() {
		obj, _ := client.Stat(ctx, c.src)
		isBatch = obj != nil && obj.Type.IsDir()
	}

	excludePatterns, err := createExcludesFromWildcard(c.exclude)
	if err != nil {
		printError(c.fullCommand, c.op, err)
		return err
	}

	for object := range objch {
		if object.Type.IsDir() || errorpkg.IsCancelation(object.Err) {
			continue
		}

		if err := object.Err; err != nil {
			merrorObjects = multierror.Append(merrorObjects, err)
			printError(c.fullCommand, c.op, err)
			continue
		}

		if object.StorageClass.IsGlacier() && !c.forceGlacierTransfer {
			if !c.ignoreGlacierWarnings {
				err := fmt.Errorf("object '%v' is on Glacier storage", object)
				merrorObjects = multierror.Append(merrorObjects, err)
				printError(c.fullCommand, c.op, err)
			}
			continue
		}

		if isURLExcluded(excludePatterns, object.URL.Path, c.src.Prefix) {
			continue
		}

		srcurl := object.URL
		var task parallel.Task

		switch {
		case srcurl.Type == c.dst.Type: // local->local or remote->remote
			task = c.prepareCopyTask(ctx, srcurl, c.dst, isBatch)
		case srcurl.IsRemote(): // remote->local
			task = c.prepareDownloadTask(ctx, srcurl, c.dst, isBatch)
		case c.dst.IsRemote(): // local->remote
			task = c.prepareUploadTask(ctx, srcurl, c.dst, isBatch)
		default:
			panic("unexpected src-dst pair")
		}

		parallel.Run(task, waiter)
	}

	waiter.Wait()
	<-errDoneCh

	return multierror.Append(merrorWaiter, merrorObjects).ErrorOrNil()
}

func (c Copy) prepareCopyTask(
	ctx context.Context,
	srcurl *url.URL,
	dsturl *url.URL,
	isBatch bool,
) func() error {
	return func() error {
		dsturl = prepareRemoteDestination(srcurl, dsturl, c.flatten, isBatch)
		err := c.doCopy(ctx, srcurl, dsturl)
		if err != nil {
			return &errorpkg.Error{
				Op:  c.op,
				Src: srcurl,
				Dst: dsturl,
				Err: err,
			}
		}
		return nil
	}
}

func (c Copy) prepareDownloadTask(
	ctx context.Context,
	srcurl *url.URL,
	dsturl *url.URL,
	isBatch bool,
) func() error {
	return func() error {
		dsturl, err := prepareLocalDestination(ctx, srcurl, dsturl, c.flatten, isBatch, c.storageOpts)
		if err != nil {
			return err
		}
		err = c.doDownload(ctx, srcurl, dsturl)
		if err != nil {
			return &errorpkg.Error{
				Op:  c.op,
				Src: srcurl,
				Dst: dsturl,
				Err: err,
			}
		}
		return nil
	}
}

func (c Copy) prepareUploadTask(
	ctx context.Context,
	srcurl *url.URL,
	dsturl *url.URL,
	isBatch bool,
) func() error {
	return func() error {
		dsturl = prepareRemoteDestination(srcurl, dsturl, c.flatten, isBatch)
		err := c.doUpload(ctx, srcurl, dsturl)
		if err != nil {
			return &errorpkg.Error{
				Op:  c.op,
				Src: srcurl,
				Dst: dsturl,
				Err: err,
			}
		}
		return nil
	}
}

// doDownload is used to fetch a remote object and save as a local object.
func (c Copy) doDownload(ctx context.Context, srcurl *url.URL, dsturl *url.URL) error {
	srcClient, err := storage.NewRemoteClient(ctx, srcurl, c.storageOpts)
	if err != nil {
		return err
	}

	dstClient := storage.NewLocalClient(c.storageOpts)

	err = c.shouldOverride(ctx, srcurl, dsturl)
	if err != nil {
		// FIXME(ig): rename
		if errorpkg.IsWarning(err) {
			printDebug(c.op, err, srcurl, dsturl)
			return nil
		}
		return err
	}

	dstPath := filepath.Dir(dsturl.Absolute())
	dstFile := filepath.Base(dsturl.Absolute())
	file, err := dstClient.CreateTemp(dstPath, dstFile)
	if err != nil {
		return err
	}

	size, err := srcClient.Get(ctx, srcurl, file, c.concurrency, c.partSize)
	file.Close()
	if err != nil {
		dErr := dstClient.Delete(ctx, &url.URL{Path: file.Name(), Type: dsturl.Type})
		if dErr != nil {
			printDebug(c.op, dErr, srcurl, dsturl)
		}
		return err
	}

	if c.deleteSource {
		_ = srcClient.Delete(ctx, srcurl)
	}

	err = dstClient.Rename(file, dsturl.Absolute())
	if err != nil {
		return err
	}

	msg := log.InfoMessage{
		Operation:   c.op,
		Source:      srcurl,
		Destination: dsturl,
		Object: &storage.Object{
			Size: size,
		},
	}
	log.Info(msg)

	return nil
}

func (c Copy) doUpload(ctx context.Context, srcurl *url.URL, dsturl *url.URL) error {
	srcClient := storage.NewLocalClient(c.storageOpts)

	file, err := srcClient.Open(srcurl.Absolute())
	if err != nil {
		return err
	}
	defer file.Close()

	err = c.shouldOverride(ctx, srcurl, dsturl)
	if err != nil {
		if errorpkg.IsWarning(err) {
			printDebug(c.op, err, srcurl, dsturl)
			return nil
		}
		return err
	}

	// override destination region if set
	if c.dstRegion != "" {
		c.storageOpts.SetRegion(c.dstRegion)
	}
	dstClient, err := storage.NewRemoteClient(ctx, dsturl, c.storageOpts)
	if err != nil {
		return err
	}

	metadata := storage.NewMetadata().
		SetStorageClass(string(c.storageClass)).
		SetSSE(c.encryptionMethod).
		SetSSEKeyID(c.encryptionKeyID).
		SetACL(c.acl).
		SetCacheControl(c.cacheControl).
		SetExpires(c.expires)

	if c.contentType != "" {
		metadata.SetContentType(c.contentType)
	} else {
		metadata.SetContentType(guessContentType(file))
	}

	if c.contentEncoding != "" {
		metadata.SetContentEncoding(c.contentEncoding)
	}

	err = dstClient.Put(ctx, file, dsturl, metadata, c.concurrency, c.partSize)
	if err != nil {
		return err
	}

	obj, _ := srcClient.Stat(ctx, srcurl)
	size := obj.Size

	if c.deleteSource {
		// close the file before deleting
		file.Close()
		if err := srcClient.Delete(ctx, srcurl); err != nil {
			return err
		}
	}

	msg := log.InfoMessage{
		Operation:   c.op,
		Source:      srcurl,
		Destination: dsturl,
		Object: &storage.Object{
			Size:         size,
			StorageClass: c.storageClass,
		},
	}
	log.Info(msg)

	return nil
}

func (c Copy) doCopy(ctx context.Context, srcurl, dsturl *url.URL) error {
	// override destination region if set
	if c.dstRegion != "" {
		c.storageOpts.SetRegion(c.dstRegion)
	}
	dstClient, err := storage.NewClient(ctx, dsturl, c.storageOpts)
	if err != nil {
		return err
	}

	metadata := storage.NewMetadata().
		SetStorageClass(string(c.storageClass)).
		SetSSE(c.encryptionMethod).
		SetSSEKeyID(c.encryptionKeyID).
		SetACL(c.acl).
		SetCacheControl(c.cacheControl).
		SetExpires(c.expires)

	if c.contentType != "" {
		metadata.SetContentType(c.contentType)
	}
	if c.contentEncoding != "" {
		metadata.SetContentEncoding(c.contentEncoding)
	}

	err = c.shouldOverride(ctx, srcurl, dsturl)
	if err != nil {
		if errorpkg.IsWarning(err) {
			printDebug(c.op, err, srcurl, dsturl)
			return nil
		}
		return err
	}

	err = dstClient.Copy(ctx, srcurl, dsturl, metadata)
	if err != nil {
		return err
	}

	if c.deleteSource {
		srcClient, err := storage.NewClient(ctx, srcurl, c.storageOpts)
		if err != nil {
			return err
		}
		if err := srcClient.Delete(ctx, srcurl); err != nil {
			return err
		}
	}

	msg := log.InfoMessage{
		Operation:   c.op,
		Source:      srcurl,
		Destination: dsturl,
		Object: &storage.Object{
			URL:          dsturl,
			StorageClass: c.storageClass,
		},
	}
	log.Info(msg)

	return nil
}

// shouldOverride function checks if the destination should be overridden if
// the source-destination pair and given copy flags conform to the
// override criteria. For example; "cp -n -s <src> <dst>" should not override
// the <dst> if <src> and <dst> filenames are the same, except if the size
// differs.
func (c Copy) shouldOverride(ctx context.Context, srcurl *url.URL, dsturl *url.URL) error {
	// if not asked to override, ignore.
	if !c.noClobber && !c.ifSizeDiffer && !c.ifSourceNewer {
		return nil
	}

	srcClient, err := storage.NewClient(ctx, srcurl, c.storageOpts)
	if err != nil {
		return err
	}

	srcObj, err := getObject(ctx, srcurl, srcClient)
	if err != nil {
		return err
	}

	dstClient, err := storage.NewClient(ctx, dsturl, c.storageOpts)
	if err != nil {
		return err
	}

	dstObj, err := getObject(ctx, dsturl, dstClient)
	if err != nil {
		return err
	}

	// if destination not exists, no conditions apply.
	if dstObj == nil {
		return nil
	}

	var stickyErr error
	if c.noClobber {
		stickyErr = errorpkg.ErrObjectExists
	}

	if c.ifSizeDiffer {
		if srcObj.Size == dstObj.Size {
			stickyErr = errorpkg.ErrObjectSizesMatch
		} else {
			stickyErr = nil
		}
	}

	if c.ifSourceNewer {
		srcMod, dstMod := srcObj.ModTime, dstObj.ModTime

		if !srcMod.After(*dstMod) {
			stickyErr = errorpkg.ErrObjectIsNewer
		} else {
			stickyErr = nil
		}
	}

	return stickyErr
}

// prepareRemoteDestination will return a new destination URL for
// remote->remote and local->remote copy operations.
func prepareRemoteDestination(
	srcurl *url.URL,
	dsturl *url.URL,
	flatten bool,
	isBatch bool,
) *url.URL {
	objname := srcurl.Base()
	if isBatch && !flatten {
		objname = srcurl.Relative()
	}

	if dsturl.IsPrefix() || dsturl.IsBucket() {
		dsturl = dsturl.Join(objname)
	}
	return dsturl
}

// prepareDownloadDestination will return a new destination URL for
// remote->local copy operations.
func prepareLocalDestination(
	ctx context.Context,
	srcurl *url.URL,
	dsturl *url.URL,
	flatten bool,
	isBatch bool,
	storageOpts storage.Options,
) (*url.URL, error) {
	objname := srcurl.Base()
	if isBatch && !flatten {
		objname = srcurl.Relative()
	}

	client := storage.NewLocalClient(storageOpts)

	if isBatch {
		err := client.MkdirAll(dsturl.Absolute())
		if err != nil {
			return nil, err
		}
	}

	obj, err := client.Stat(ctx, dsturl)

	if err != nil {
		var objNotFound *storage.ErrGivenObjectNotFound
		if !errors.As(err, &objNotFound) {
			return nil, err
		}
	}

	if isBatch && !flatten {
		dsturl = dsturl.Join(objname)
		err := client.MkdirAll(dsturl.Dir())
		if err != nil {
			return nil, err
		}
	}
	var objNotFound *storage.ErrGivenObjectNotFound
	if errors.As(err, &objNotFound) {
		err := client.MkdirAll(dsturl.Dir())
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(dsturl.Absolute(), "/") {
			dsturl = dsturl.Join(objname)
		}
	} else {
		if obj.Type.IsDir() {
			dsturl = obj.URL.Join(objname)
		}
	}

	return dsturl, nil
}

// getObject checks if the object from given url exists. If no object is
// found, error and returning object would be nil.
func getObject(ctx context.Context, url *url.URL, client storage.Storage) (*storage.Object, error) {
	obj, err := client.Stat(ctx, url)
	var objNotFound *storage.ErrGivenObjectNotFound
	if errors.As(err, &objNotFound) {
		return nil, nil
	}

	return obj, err
}

func validateCopyCommand(c *cli.Context) error {
	if c.Args().Len() != 2 {
		return fmt.Errorf("expected source and destination arguments")
	}

	ctx := c.Context
	src := c.Args().Get(0)
	dst := c.Args().Get(1)

	srcurl, err := url.New(src, url.WithVersion(c.String("version-id")),
		url.WithRaw(c.Bool("raw")))
	if err != nil {
		return err
	}

	dsturl, err := url.New(dst, url.WithRaw(c.Bool("raw")))
	if err != nil {
		return err
	}

	// wildcard destination doesn't mean anything
	if dsturl.IsWildcard() {
		return fmt.Errorf("target %q can not contain glob characters", dst)
	}

	// we don't operate on S3 prefixes for copy and delete operations.
	if srcurl.IsBucket() || srcurl.IsPrefix() {
		return fmt.Errorf("source argument must contain wildcard character")
	}

	// 'cp dir/* s3://bucket/prefix': expect a trailing slash to avoid any
	// surprises.
	if srcurl.IsWildcard() && dsturl.IsRemote() && !dsturl.IsPrefix() && !dsturl.IsBucket() {
		return fmt.Errorf("target %q must be a bucket or a prefix", dsturl)
	}

	if err := checkVersinoningURLRemote(srcurl); err != nil {
		return err
	}

	if err := checkVersioningWithGoogleEndpoint(c); err != nil {
		return err
	}

	switch {
	case srcurl.Type == dsturl.Type:
		return validateCopy(srcurl, dsturl)
	case dsturl.IsRemote():
		return validateUpload(ctx, srcurl, dsturl, NewStorageOpts(c))
	default:
		return nil
	}
}

func validateCopy(srcurl, dsturl *url.URL) error {
	if srcurl.IsRemote() || dsturl.IsRemote() {
		return nil
	}

	// we don't support local->local copies
	return fmt.Errorf("local->local copy operations are not permitted")
}

func validateUpload(ctx context.Context, srcurl, dsturl *url.URL, storageOpts storage.Options) error {
	srcclient := storage.NewLocalClient(storageOpts)

	if srcurl.IsWildcard() {
		return nil
	}

	obj, err := srcclient.Stat(ctx, srcurl)
	if err != nil {
		return err
	}

	// 'cp dir/ s3://bucket/prefix-without-slash': expect a trailing slash to
	// avoid any surprises.
	if obj.Type.IsDir() && !dsturl.IsBucket() && !dsturl.IsPrefix() {
		return fmt.Errorf("target %q must be a bucket or a prefix", dsturl)
	}

	return nil
}

// guessContentType gets content type of the file.
func guessContentType(file *os.File) string {
	contentType := mime.TypeByExtension(filepath.Ext(file.Name()))
	if contentType == "" {
		defer file.Seek(0, io.SeekStart)

		const bufsize = 512
		buf, err := io.ReadAll(io.LimitReader(file, bufsize))
		if err != nil {
			return ""
		}

		return http.DetectContentType(buf)
	}
	return contentType
}
