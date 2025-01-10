package akave

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"path"
	"strconv"
	"strings"
	"time"

	"akave.ai/akavesdk/sdk"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/hash"
)



var (
	errorReadOnly = errors.New("temp error for implementation")
	timeUnset     = time.Unix(0, 0)
)



// akaveCommandHelp provides detailed help for the Akave backend.
var akaveCommandHelp = []fs.CommandHelp{
   {
       Name:  "Configure Akave Backend",
       Short: "Setup Akave storage backend",
       Long: `Allows access to Akave Storage. You can use Akave as a remote storage backend with rclone by configuring it using:

rclone config

Follow the prompts to set up your Akave remote.

**Examples:**

- **List Buckets:**
` + "```" + `
rclone lsd akave:
` + "```" + `

- **Upload a File:**
` + "```" + `
rclone copy /path/to/local/file akave:bucketname
` + "```" + `


Ensure that you have the necessary permissions and that the endpoint URL is correct.`,
       Opts: map[string]string{
           "node_url":         "API endpoint for Akave storage. E.g., \"https://api.akave.ai\".",
           "max_concurrency": "Maximum number of concurrent operations. Default is 4.",
           "block_part_size": "Size of block parts in bytes. Default is 1048576 (1 MiB).",
           "private_key":  "Private key for securing your data (optional). Must be 32 bytes long.",
       },
   },
}


func init() {
    fsi := &fs.RegInfo{
        Name:        "akave",
        Description: "Akave Storage Backend allows rclone to interact with Akave's storage services.",
        NewFs:       NewFs,
		// TODO: understand what is that(probably the help section)
        CommandHelp: akaveCommandHelp,
        Options: []fs.Option{
            {
                Name:     "node_url",
                Help:     "API ulr for Akave storage.\n\nE.g., \"https://api.akave.ai\".",
                Required: true,
            },
            {
                Name:    "max_concurrency",
                Help:    "Maximum number of concurrent operations.",
                Default: 4,
                Advanced: true,
            },
            {
                Name:    "block_part_size",
                Help: `Size of block parts in bytes.

This controls the size of chunks when uploading or downloading files. 
Must be a positive integer. Default is 1048576 (1 MiB).`,
                Default: 1048576,
                Advanced: true,
            },
            {
                Name:      "private_key",
                Help:      "Encryption key for securing your data (optional). Must be 32 bytes long.",
                IsPassword: true,
                Advanced:  true,
            },
        },
    }
    fs.Register(fsi)
}


// Fs represents a remote akave server
type Fs struct {
    name     string
    root     string
    features *fs.Features
    sdk      *sdk.SDK
}

var _ fs.Fs = (*Fs)(nil)
var _ fs.CleanUpper = (*Fs)(nil)


// Object represents a file in Akave storage
type Object struct {
    fs     *Fs
    remote string
    info   sdk.FileMeta
}

// ---------------------------------- fs interface implementation--------------------------------


func (f *Fs) Features() *fs.Features {
	return f.features
}


// TODO: understand what is this
func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.None)
}

func (f *Fs) Name() string {
	return f.name
}


func (f *Fs) Precision() time.Duration {
	return time.Nanosecond
}


// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// TODO: go over this: String converts this Fs to a string
func (f *Fs) String() string {
	if f.root == "" {
		return "akave root"
	}
	return fmt.Sprintf("A bucket path %s", f.root)
}

// ---------------------------------- fs interface --------------------------------


// NewFs constructs an Fs from the path, bucket:path
func NewFs(ctx context.Context,name, root string, m configmap.Mapper) (fs.Fs, error) {
    // Retrieve endpoint - this one is required, so return an error if empty
    node_url, _ := m.Get("node_url")
    if node_url == "" {
        return nil, errors.New("node_url is required")
    }

    // Optional max concurrency, default to 4 if unset or invalid
    maxConcurrencyStr, _ := m.Get("max_concurrency")
    maxConcurrency, err := strconv.Atoi(maxConcurrencyStr)
    if err != nil || maxConcurrency <= 0 {
        maxConcurrency = 4
    }

    // Optional block part size, default to 1 MiB if unset or invalid
    blockPartSizeStr, _ := m.Get("block_part_size")
    blockPartSize, err := strconv.ParseInt(blockPartSizeStr, 10, 64)
    if err != nil || blockPartSize <= 0 {
        blockPartSize = 1048576 // 1 MiB
    }

    // Optional encryption key
    privateKeyStr, _ := m.Get("private_key") 
    // Initialize the SDK with the configuration
    akaveSDK, err := sdk.New(node_url, maxConcurrency, blockPartSize, false, sdk.WithPrivateKey(privateKeyStr))
    if err != nil {
        return nil, err
    }

    // Initialize your backend (Fs)
    f := &Fs{
        name: name,
        root: root,
        sdk:  akaveSDK,
    }
	// TODO understahnd what is this
	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
        BucketBased:             true,  // **Enabled**
        BucketBasedRootOK:       true,  // **Enabled**
	}).Fill(ctx, f)

    return f, nil
}


// List the objects and directories in dir into entries
func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {    
    bucketName := f.root
    if bucketName == "" {
        return f.listBuckets(ctx)
    }

    return f.listFilesInDirectory(ctx, bucketName)
}

// Put the object into the bucket
//
// Copy the reader in to the new object which is returned.
//
// The new object may have been created if an error is returned
// Put first call NewObject to check if the object exsits in the system. If the object exists, it would call the Update method
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
    ipc, err := f.sdk.IPC()
    if err != nil {
        return nil, err
    }
    
    remote := src.Remote()
    size := src.Size()
    modTime := src.ModTime(ctx)


    // Handle unknown-sized objects
    if size < 0 {
        return nil, errors.New("akave: unknown object size is not supported")
    }
   

    bucketName := f.root
    fileName := remote // Assuming 'remote' is the path within the bucket


    fileUpload, err := ipc.CreateFileUpload(ctx, bucketName, fileName, size, in)
    if err != nil {

        fileInfo, fetchErr := f.sdk.FileInfo(ctx, bucketName, fileName)
        if fetchErr == nil {
            obj := f.newObject(fileName, fileInfo)
            return obj, fmt.Errorf("akave: upload failed for '%s' but object was created: %w", fileName, err)
        }
        return nil, fmt.Errorf("akave: failed to upload file '%s': %w", fileName, err)
    }

   
    fileMeta := sdk.FileMeta{
        RootCID:   fileUpload.RootCID,
        Name:      remote,
        Size:      size,
        CreatedAt: modTime,
    }

    // Return the new Object instance
    return f.newObject(fileName, fileMeta), nil
}



// TODO: maybe add valiation that there is not file and that the bucket exists(that would take more time)
// this won't be needed if the backend would provide a detailed error message
func (f *Fs) Rmdir(ctx context.Context, bucketName string) error {
	err := f.sdk.DeleteBucket(ctx, bucketName)
    if err != nil {
        return fmt.Errorf("akave: failed to delete bucket '%s': %w", bucketName, err)
    }

    return nil
}

// TODO: create the bucket
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	return errorReadOnly
}


// Helper function to list all buckets
func (f *Fs) listBuckets(ctx context.Context) (fs.DirEntries, error) {
    ipc, err := f.sdk.IPC()
    if err != nil {
        return nil, err
    }

    // Fetch the list of buckets
    buckets, err := ipc.ListBuckets(ctx)
    if err != nil {
        return nil, fmt.Errorf("akave: failed to list buckets: %w", err)
    }

    // Convert the list of buckets to fs.DirEntries
    var entries fs.DirEntries
    for _, bucket := range buckets {
        entries = append(entries, fs.NewDir(bucket.Name, bucket.CreatedAt))
    }

    return entries, nil
}

// Helper function to list files in a bucket
func (f *Fs) listFilesInBucket(ctx context.Context, bucketName string) (fs.DirEntries, error) {
    files, err := f.sdk.ListFiles(ctx, bucketName)
    if err != nil {
        return nil, err
    }

    var entries fs.DirEntries
    for _, file := range files {
        remote := path.Join(bucketName, file.Name)
		fileMeta := sdk.FileMeta{
			RootCID: file.RootCID,
			Name: file.Name,
			Size: file.Size,
			CreatedAt: file.CreatedAt,
		}
        obj := f.newObject(remote, fileMeta)
        entries = append(entries, obj)
    }

    return entries, nil
}

// Helper function to list files in a subdirectory
func (f *Fs) listFilesInDirectory(ctx context.Context, dir string) (fs.DirEntries, error) {
    ipc, err := f.sdk.IPC()
    if err != nil {
        return nil, err
    }
    // List files relative to the current directory
    files, err := ipc.ListFiles(ctx, dir)
    if err != nil {
        return nil, fmt.Errorf("akave: failed to list files in '%s': %w", dir, fs.ErrorDirNotFound)
    }

    var entries fs.DirEntries

    for _, file := range files {
        remote := file.Name

        fileMeta := sdk.FileMeta{
            RootCID:   file.RootCID,
            Name:      remote,
            Size:      file.Size,
            CreatedAt: file.CreatedAt,
        }
        obj := f.newObject(remote, fileMeta)
        entries = append(entries, obj)
  
    }

    return entries, nil
}

// NewObject fetches the object from the remote path.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {


    ipc, err := f.sdk.IPC()
    if err != nil {
        return nil, err
    }
    
    bucketName := f.Root()
    // Fetch file information using the SDK
    fileInfo, err := ipc.FileInfo(ctx, bucketName, remote)
    if err != nil {
        return nil, fs.ErrorObjectNotFound
    }

    // Create and return the Object
    return &Object{
        fs:     f,
        remote: remote,
        info:   fileInfo,
    }, nil
}




func (f *Fs) newObject(remote string, fileInfo sdk.FileMeta)  fs.Object {
	return &Object{
        fs:     f,
        remote: remote,
        info:   fileInfo,
    } 
}

// fullPath returns the full path by joining root and dir
func (f *Fs) fullPath(dir string) string {
    if f.root == "" {
        return dir
    }
    if dir == "" {
        return f.root
    }
    return path.Join(f.root, dir)
}

// RemoveDuplicateDirs removes duplicate directories from DirEntries
func RemoveDuplicateDirs(entries fs.DirEntries) fs.DirEntries {
    seen := make(map[string]struct{})
    var result fs.DirEntries
    for _, entry := range entries {
        if dir, ok := entry.(fs.Directory); ok {
            if _, found := seen[dir.Remote()]; !found {
                seen[dir.Remote()] = struct{}{}
                result = append(result, dir)
            }
        } else {
            result = append(result, entry)
        }
    }
    return result
}

// Remove method for completeness (not used in this example)
func (f *Fs) Remove(ctx context.Context, dir string) error {
    return nil
}

// Implement the Object interface methods...

// --------------------------------- Object methods -----------------------------

// Fs returns the parent Fs
func (o *Object) Fs() fs.Info {
    return o.fs
}

// Return a string version
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
    // TODO: Implement actual opening logic
    return ioutil.NopCloser(bytes.NewReader([]byte{})), nil
}
// Remote returns the remote path when printing the object
func (o *Object) Remote() string {
    return o.remote
}
// Precision of the remote(like s3)
// ModTime returns the modification time
func (o *Object) ModTime(ctx context.Context) time.Time {
    return o.info.CreatedAt
}

// Size returns the size of the object
func (o *Object) Size() int64 {
    return o.info.Size
}

// Hash returns the hash of the object (not implemented)
func (o *Object) Hash(ctx context.Context, ty hash.Type) (string, error) {
    return "", hash.ErrUnsupported
}

// Storable indicates whether the object can be stored (always true)
func (o *Object) Storable() bool {
    return true
}

// SetModTime sets the modification time (not implemented)
func (o *Object) SetModTime(ctx context.Context, t time.Time) error {
    return fs.ErrorCantSetModTime
}



// Update updates the object with the contents of the reader (not implemented)
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
    fmt.Println("trying to execute Update!")
	return errorReadOnly
}


// Remove removes the object
func (o *Object) Remove(ctx context.Context) error {
    return o.fs.sdk.FileDelete(ctx, o.fs.bucketNameFromRemote(o.remote), o.fileNameFromRemote(o.remote))
}

// Helper methods to extract bucket and file names
func (f *Fs) bucketNameFromRemote(remote string) string {
    parts := strings.SplitN(remote, "/", 2)
    return parts[0]
}

func (o *Object) fileNameFromRemote(remote string) string {
    parts := strings.SplitN(remote, "/", 2)
    if len(parts) > 1 {
        return parts[1]
    }
    return ""
}

// ------------------------------------------------------------------------------------

func (f *Fs) split(remote string) (bucket, file string) {
    parts := strings.SplitN(remote, "/", 2)
    if len(parts) < 2 {
        return "", ""
    }
    bucket = parts[0]
    file = parts[1]
    return
}
func (f *Fs) CleanUp(ctx context.Context) error {
    fmt.Println("running close function!!")
    if f.sdk != nil {
        return f.sdk.Close()
    }
    return nil
}
