// Copyright 2015 Ka-Hing Cheung
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package internal

import (
	"fmt"
	"log"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/context"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"

	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"
)

// goofys is a Filey System written in Go. All the backend data is
// stored on S3 as is. It's a Filey System instead of a File System
// because it makes minimal effort at being POSIX
// compliant. Particularly things that are difficult to support on S3
// or would translate into more than one round-trip would either fail
// (rename non-empty dir) or faked (no per-file permission). goofys
// does not have a on disk data cache, and consistency model is
// close-to-open.

type Goofys struct {
	fuseutil.NotImplementedFileSystem
	bucket string

	flags *FlagStorage

	umask uint32

	awsConfig *aws.Config
	s3        *s3.S3
	rootAttrs fuseops.InodeAttributes

	bufferPool *BufferPool

	// A lock protecting the state of the file system struct itself (distinct
	// from per-inode locks). Make sure to see the notes on lock ordering above.
	mu sync.Mutex

	// The next inode ID to hand out. We assume that this will never overflow,
	// since even if we were handing out inode IDs at 4 GHz, it would still take
	// over a century to do so.
	//
	// GUARDED_BY(mu)
	nextInodeID fuseops.InodeID

	// The collection of live inodes, keyed by inode ID. No ID less than
	// fuseops.RootInodeID is ever used.
	//
	// INVARIANT: For all keys k, fuseops.RootInodeID <= k < nextInodeID
	// INVARIANT: For all keys k, inodes[k].ID() == k
	// INVARIANT: inodes[fuseops.RootInodeID] is missing or of type inode.DirInode
	// INVARIANT: For all v, if IsDirName(v.Name()) then v is inode.DirInode
	//
	// GUARDED_BY(mu)
	inodes      map[fuseops.InodeID]*Inode
	inodesCache map[string]*Inode // fullname to inode

	nextHandleID fuseops.HandleID
	dirHandles   map[fuseops.HandleID]*DirHandle

	fileHandles map[fuseops.HandleID]*FileHandle
}

func NewGoofys(bucket string, awsConfig *aws.Config, flags *FlagStorage) *Goofys {
	// Set up the basic struct.
	fs := &Goofys{
		bucket: bucket,
		flags:  flags,
		umask:  0122,
	}

	if flags.DebugS3 {
		awsConfig.LogLevel = aws.LogLevel(aws.LogDebug | aws.LogDebugWithRequestErrors)
	}

	fs.awsConfig = awsConfig
	fs.s3 = s3.New(awsConfig)

	params := &s3.GetBucketLocationInput{Bucket: &bucket}
	resp, err := fs.s3.GetBucketLocation(params)
	var fromRegion, toRegion string
	if err != nil {
		if mapAwsError(err) == fuse.ENOENT {
			log.Printf("bucket %v does not exist", bucket)
			return nil
		}
		fromRegion, toRegion = parseRegionError(err)
	} else {
		fs.logS3(resp)

		if resp.LocationConstraint == nil {
			toRegion = "us-east-1"
		} else {
			toRegion = *resp.LocationConstraint
		}

		fromRegion = *awsConfig.Region
	}

	if len(toRegion) != 0 && fromRegion != toRegion {
		log.Printf("Switching from region '%v' to '%v'", fromRegion, toRegion)
		awsConfig.Region = &toRegion
		fs.s3 = s3.New(awsConfig)
		_, err = fs.s3.GetBucketLocation(params)
		if err != nil {
			log.Println(err)
			return nil
		}
	} else if len(toRegion) == 0 && *awsConfig.Region != "milkyway" {
		log.Printf("Unable to detect bucket region, staying at '%v'", *awsConfig.Region)
	}

	now := time.Now()
	fs.rootAttrs = fuseops.InodeAttributes{
		Size:   4096,
		Nlink:  2,
		Mode:   flags.DirMode | os.ModeDir,
		Atime:  now,
		Mtime:  now,
		Ctime:  now,
		Crtime: now,
		Uid:    fs.flags.Uid,
		Gid:    fs.flags.Gid,
	}

	fs.bufferPool = NewBufferPool(1000*1024*1024, 200*1024*1024)

	fs.nextInodeID = fuseops.RootInodeID + 1
	fs.inodes = make(map[fuseops.InodeID]*Inode)
	root := NewInode(aws.String(""), aws.String(""), flags)
	root.Id = fuseops.RootInodeID
	root.Attributes = &fs.rootAttrs

	fs.inodes[fuseops.RootInodeID] = root
	fs.inodesCache = make(map[string]*Inode)

	fs.nextHandleID = 1
	fs.dirHandles = make(map[fuseops.HandleID]*DirHandle)

	fs.fileHandles = make(map[fuseops.HandleID]*FileHandle)

	return fs
}

// Find the given inode. Panic if it doesn't exist.
//
// LOCKS_REQUIRED(fs.mu)
func (fs *Goofys) getInodeOrDie(id fuseops.InodeID) (inode *Inode) {
	inode = fs.inodes[id]
	if inode == nil {
		panic(fmt.Sprintf("Unknown inode: %v", id))
	}

	return
}

func (fs *Goofys) logFuse(op string, args ...interface{}) {
	if fs.flags.DebugFuse {
		log.Printf("%v: %v", op, args)
	}
}

func (fs *Goofys) logS3(resp ...interface{}) {
	if fs.flags.DebugS3 {
		log.Println(resp)
	}
}

func (fs *Goofys) StatFS(
	ctx context.Context,
	op *fuseops.StatFSOp) (err error) {

	const BLOCK_SIZE = 4096
	const TOTAL_SPACE = 1 * 1024 * 1024 * 1024 * 1024 * 1024 // 1PB
	const TOTAL_BLOCKS = TOTAL_SPACE / BLOCK_SIZE
	const INODES = 1 * 1000 * 1000 * 1000 // 1 billion
	op.BlockSize = BLOCK_SIZE
	op.Blocks = TOTAL_BLOCKS
	op.BlocksFree = TOTAL_BLOCKS
	op.BlocksAvailable = TOTAL_BLOCKS
	op.IoSize = 1 * 1024 * 1024 // 1MB
	op.Inodes = INODES
	op.InodesFree = INODES
	return
}

func (fs *Goofys) GetInodeAttributes(
	ctx context.Context,
	op *fuseops.GetInodeAttributesOp) (err error) {

	fs.mu.Lock()
	inode := fs.getInodeOrDie(op.Inode)
	fs.mu.Unlock()

	attr, err := inode.GetAttributes(fs)
	op.Attributes = *attr
	op.AttributesExpiration = time.Now().Add(365 * 24 * time.Hour)

	return
}

const REGION_ERROR_MSG = "The authorization header is malformed; the region %s is wrong; expecting %s"

func parseRegionError(err error) (fromRegion, toRegion string) {
	if reqErr, ok := err.(awserr.RequestFailure); ok {
		// A service error occurred
		if reqErr.StatusCode() == 400 && reqErr.Code() == "AuthorizationHeaderMalformed" {
			n, scanErr := fmt.Sscanf(reqErr.Message(), REGION_ERROR_MSG, &fromRegion, &toRegion)
			if n != 2 || scanErr != nil {
				fmt.Println(n, scanErr)
				return
			}
			fromRegion = fromRegion[1 : len(fromRegion)-1]
			toRegion = toRegion[1 : len(toRegion)-1]
			return
		} else if reqErr.StatusCode() == 501 {
			// method not implemented,
			// do nothing, use existing region
		} else {
			log.Printf("code=%v msg=%v request=%v\n",
				reqErr.Message(), reqErr.StatusCode(), reqErr.RequestID())
		}
	}
	return
}

func mapAwsError(err error) error {
	if awsErr, ok := err.(awserr.Error); ok {
		if reqErr, ok := err.(awserr.RequestFailure); ok {
			// A service error occurred
			switch reqErr.StatusCode() {
			case 404:
				return fuse.ENOENT
			case 405:
				return syscall.ENOTSUP
			default:
				log.Printf("code=%v msg=%v request=%v\n", reqErr.Message(), reqErr.StatusCode(), reqErr.RequestID())
				return reqErr
			}
		} else {
			// Generic AWS Error with Code, Message, and original error (if any)
			log.Printf("code=%v msg=%v, err=%v\n", awsErr.Code(), awsErr.Message(), awsErr.OrigErr())
			return awsErr
		}
	} else {
		return err
	}
}

func (fs *Goofys) LookUpInodeNotDir(name string, c chan s3.HeadObjectOutput, errc chan error) {
	params := &s3.HeadObjectInput{Bucket: &fs.bucket, Key: &name}
	resp, err := fs.s3.HeadObject(params)
	if err != nil {
		errc <- mapAwsError(err)
		return
	}

	fs.logS3(resp)
	c <- *resp
}

func (fs *Goofys) LookUpInodeDir(name string, c chan s3.ListObjectsOutput, errc chan error) {
	params := &s3.ListObjectsInput{
		Bucket:    &fs.bucket,
		Delimiter: aws.String("/"),
		MaxKeys:   aws.Int64(1),
		Prefix:    aws.String(name + "/"),
	}

	resp, err := fs.s3.ListObjects(params)
	if err != nil {
		errc <- mapAwsError(err)
		return
	}

	fs.logS3(resp)
	c <- *resp
}

func (fs *Goofys) mpuCopyPart(from string, to string, mpuId string, bytes string, part int64, wg *sync.WaitGroup,
	etag **string, errout *error) {

	defer func() {
		wg.Done()
	}()

	// XXX use CopySourceIfUnmodifiedSince to ensure that
	// we are copying from the same object
	params := &s3.UploadPartCopyInput{
		Bucket:          &fs.bucket,
		Key:             &to,
		CopySource:      &from,
		UploadId:        &mpuId,
		CopySourceRange: &bytes,
		PartNumber:      &part,
	}

	fs.logS3(params)

	resp, err := fs.s3.UploadPartCopy(params)
	if err != nil {
		*errout = mapAwsError(err)
		return
	}

	*etag = resp.CopyPartResult.ETag
	return
}

func sizeToParts(size int64) int {
	const PART_SIZE = 5 * 1024 * 1024 * 1024

	nParts := int(size / PART_SIZE)
	if size%PART_SIZE != 0 {
		nParts++
	}
	return nParts
}

func (fs *Goofys) mpuCopyParts(size int64, from string, to string, mpuId string,
	wg *sync.WaitGroup, etags []*string, err *error) {

	const PART_SIZE = 5 * 1024 * 1024 * 1024

	rangeFrom := int64(0)
	rangeTo := int64(0)

	for i := int64(1); rangeTo < size; i++ {
		rangeFrom = rangeTo
		rangeTo = i * PART_SIZE
		if rangeTo > size {
			rangeTo = size
		}
		bytes := fmt.Sprintf("bytes=%v-%v", rangeFrom, rangeTo-1)

		wg.Add(1)
		go fs.mpuCopyPart(from, to, mpuId, bytes, i, wg, &etags[i-1], err)
	}
}

func (fs *Goofys) copyObjectMultipart(size int64, from string, to string, mpuId string) (err error) {
	var wg sync.WaitGroup
	nParts := sizeToParts(size)
	etags := make([]*string, nParts)

	if mpuId == "" {
		params := &s3.CreateMultipartUploadInput{
			Bucket:       &fs.bucket,
			Key:          &to,
			StorageClass: &fs.flags.StorageClass,
		}

		resp, err := fs.s3.CreateMultipartUpload(params)
		if err != nil {
			return mapAwsError(err)
		}

		mpuId = *resp.UploadId
	}

	fs.mpuCopyParts(size, from, to, mpuId, &wg, etags, &err)
	wg.Wait()

	if err != nil {
		return
	} else {
		parts := make([]*s3.CompletedPart, nParts)
		for i := 0; i < nParts; i++ {
			parts[i] = &s3.CompletedPart{
				ETag:       etags[i],
				PartNumber: aws.Int64(int64(i + 1)),
			}
		}

		params := &s3.CompleteMultipartUploadInput{
			Bucket:   &fs.bucket,
			Key:      &to,
			UploadId: &mpuId,
			MultipartUpload: &s3.CompletedMultipartUpload{
				Parts: parts,
			},
		}

		fs.logS3(params)

		_, err = fs.s3.CompleteMultipartUpload(params)
		if err != nil {
			return mapAwsError(err)
		}
	}

	return
}

func (fs *Goofys) copyObjectMaybeMultipart(size int64, from string, to string) (err error) {
	if size == -1 {
		params := &s3.HeadObjectInput{Bucket: &fs.bucket, Key: &from}
		resp, err := fs.s3.HeadObject(params)
		if err != nil {
			return mapAwsError(err)
		}

		size = *resp.ContentLength
	}

	from = fs.bucket + "/" + from

	if size > 5*1024*1024*1024 {
		return fs.copyObjectMultipart(size, from, to, "")
	}

	params := &s3.CopyObjectInput{
		Bucket:       &fs.bucket,
		CopySource:   &from,
		Key:          &to,
		StorageClass: &fs.flags.StorageClass,
	}

	_, err = fs.s3.CopyObject(params)
	if err != nil {
		err = mapAwsError(err)
	}

	return
}

func (fs *Goofys) allocateInodeId() (id fuseops.InodeID) {
	id = fs.nextInodeID
	fs.nextInodeID++
	return
}

// returned inode has nil Id
func (fs *Goofys) LookUpInodeMaybeDir(name string, fullName string) (inode *Inode, err error) {
	errObjectChan := make(chan error, 1)
	objectChan := make(chan s3.HeadObjectOutput, 1)
	errDirChan := make(chan error, 1)
	dirChan := make(chan s3.ListObjectsOutput, 1)

	go fs.LookUpInodeNotDir(fullName, objectChan, errObjectChan)
	go fs.LookUpInodeDir(fullName, dirChan, errDirChan)

	notFound := false

	for {
		select {
		case resp := <-objectChan:
			// XXX/TODO if both object and object/ exists, return dir
			inode = NewInode(&name, &fullName, fs.flags)
			inode.Attributes = &fuseops.InodeAttributes{
				Size:   uint64(*resp.ContentLength),
				Nlink:  1,
				Mode:   fs.flags.FileMode,
				Atime:  *resp.LastModified,
				Mtime:  *resp.LastModified,
				Ctime:  *resp.LastModified,
				Crtime: *resp.LastModified,
				Uid:    fs.flags.Uid,
				Gid:    fs.flags.Gid,
			}
			return
		case err = <-errObjectChan:
			if err == fuse.ENOENT {
				if notFound {
					return nil, err
				} else {
					notFound = true
					err = nil
				}
			} else {
				// XXX retry
			}
		case resp := <-dirChan:
			if len(resp.CommonPrefixes) != 0 || len(resp.Contents) != 0 {
				inode = NewInode(&name, &fullName, fs.flags)
				inode.Attributes = &fs.rootAttrs
				return
			} else {
				// 404
				if notFound {
					return nil, fuse.ENOENT
				} else {
					notFound = true
				}
			}
		case err = <-errDirChan:
			// XXX retry
		}
	}
}

func (fs *Goofys) LookUpInode(
	ctx context.Context,
	op *fuseops.LookUpInodeOp) (err error) {

	fs.mu.Lock()

	parent := fs.getInodeOrDie(op.Parent)
	inode, ok := fs.inodesCache[parent.getChildName(op.Name)]
	if ok {
		defer inode.Ref()
	} else {
		fs.mu.Unlock()

		inode, err = parent.LookUp(fs, op.Name)
		if err != nil {
			return err
		}

		fs.mu.Lock()
		inode.Id = fs.allocateInodeId()
		fs.inodesCache[*inode.FullName] = inode
	}

	fs.inodes[inode.Id] = inode
	op.Entry.Child = inode.Id
	op.Entry.Attributes = *inode.Attributes
	op.Entry.AttributesExpiration = time.Now().Add(fs.flags.StatCacheTTL)
	op.Entry.EntryExpiration = time.Now().Add(fs.flags.TypeCacheTTL)
	fs.mu.Unlock()

	inode.logFuse("<-- LookUpInode")

	return
}

// LOCKS_EXCLUDED(fs.mu)
func (fs *Goofys) ForgetInode(
	ctx context.Context,
	op *fuseops.ForgetInodeOp) (err error) {

	fs.mu.Lock()
	inode := fs.getInodeOrDie(op.Inode)
	fs.mu.Unlock()

	stale := inode.DeRef(op.N)

	if stale {
		fs.mu.Lock()
		defer fs.mu.Unlock()

		delete(fs.inodes, op.Inode)
		delete(fs.inodesCache, *inode.FullName)
	}

	return
}

func (fs *Goofys) OpenDir(
	ctx context.Context,
	op *fuseops.OpenDirOp) (err error) {
	fs.mu.Lock()

	handleID := fs.nextHandleID
	fs.nextHandleID++

	in := fs.getInodeOrDie(op.Inode)
	fs.mu.Unlock()

	// XXX/is this a dir?
	dh := in.OpenDir()

	fs.mu.Lock()
	defer fs.mu.Unlock()

	fs.dirHandles[handleID] = dh
	op.Handle = handleID

	return
}

// LOCKS_EXCLUDED(fs.mu)
func (fs *Goofys) ReadDir(
	ctx context.Context,
	op *fuseops.ReadDirOp) (err error) {

	// Find the handle.
	fs.mu.Lock()
	dh := fs.dirHandles[op.Handle]
	//inode := fs.inodes[op.Inode]
	fs.mu.Unlock()

	if dh == nil {
		panic(fmt.Sprintf("can't find dh=%v", op.Handle))
	}

	dh.inode.logFuse("ReadDir", op.Offset)

	for i := op.Offset; ; i++ {
		e, err := dh.ReadDir(fs, i)
		if err != nil {
			return err
		}
		if e == nil {
			break
		}

		n := fuseutil.WriteDirent(op.Dst[op.BytesRead:], *e)
		if n == 0 {
			break
		}

		dh.inode.logFuse("<-- ReadDir", e.Name, e.Offset)

		op.BytesRead += n
	}

	return
}

func (fs *Goofys) ReleaseDirHandle(
	ctx context.Context,
	op *fuseops.ReleaseDirHandleOp) (err error) {

	fs.mu.Lock()
	defer fs.mu.Unlock()

	dh := fs.dirHandles[op.Handle]
	dh.CloseDir()
	fs.logFuse("ReleaseDirHandle", *dh.inode.FullName)

	delete(fs.dirHandles, op.Handle)

	return
}

func (fs *Goofys) OpenFile(
	ctx context.Context,
	op *fuseops.OpenFileOp) (err error) {
	fs.mu.Lock()
	in := fs.getInodeOrDie(op.Inode)
	fs.mu.Unlock()

	fh := in.OpenFile(fs)

	fs.mu.Lock()
	defer fs.mu.Unlock()

	handleID := fs.nextHandleID
	fs.nextHandleID++

	fs.fileHandles[handleID] = fh

	op.Handle = handleID
	op.KeepPageCache = true

	return
}

func (fs *Goofys) ReadFile(
	ctx context.Context,
	op *fuseops.ReadFileOp) (err error) {

	fs.mu.Lock()
	fh := fs.fileHandles[op.Handle]
	fs.mu.Unlock()

	op.BytesRead, err = fh.ReadFile(fs, op.Offset, op.Dst)

	return
}

func (fs *Goofys) SyncFile(
	ctx context.Context,
	op *fuseops.SyncFileOp) (err error) {

	fs.mu.Lock()
	fh := fs.fileHandles[op.Handle]
	fs.mu.Unlock()

	err = fh.FlushFile(fs)
	return
}

func (fs *Goofys) FlushFile(
	ctx context.Context,
	op *fuseops.FlushFileOp) (err error) {

	fs.mu.Lock()
	fh := fs.fileHandles[op.Handle]
	fs.mu.Unlock()

	err = fh.FlushFile(fs)

	return
}

func (fs *Goofys) ReleaseFileHandle(
	ctx context.Context,
	op *fuseops.ReleaseFileHandleOp) (err error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	delete(fs.fileHandles, op.Handle)
	return
}

func (fs *Goofys) CreateFile(
	ctx context.Context,
	op *fuseops.CreateFileOp) (err error) {

	fs.mu.Lock()
	parent := fs.getInodeOrDie(op.Parent)
	fs.mu.Unlock()

	inode, fh := parent.Create(fs, op.Name)

	fs.mu.Lock()
	defer fs.mu.Unlock()

	nextInode := fs.nextInodeID
	fs.nextInodeID++

	inode.Id = nextInode

	fs.inodes[inode.Id] = inode
	fs.inodesCache[*inode.FullName] = inode

	op.Entry.Child = inode.Id
	op.Entry.Attributes = *inode.Attributes
	op.Entry.AttributesExpiration = time.Now().Add(fs.flags.StatCacheTTL)
	op.Entry.EntryExpiration = time.Now().Add(fs.flags.TypeCacheTTL)

	// Allocate a handle.
	handleID := fs.nextHandleID
	fs.nextHandleID++

	fs.fileHandles[handleID] = fh

	op.Handle = handleID

	inode.logFuse("<-- CreateFile")

	return
}

func (fs *Goofys) MkDir(
	ctx context.Context,
	op *fuseops.MkDirOp) (err error) {

	fs.mu.Lock()
	parent := fs.getInodeOrDie(op.Parent)
	fs.mu.Unlock()

	// ignore op.Mode for now
	inode, err := parent.MkDir(fs, op.Name)
	if err != nil {
		return err
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	nextInode := fs.nextInodeID
	fs.nextInodeID++

	inode.Id = nextInode

	fs.inodes[inode.Id] = inode
	op.Entry.Child = inode.Id
	op.Entry.Attributes = *inode.Attributes
	op.Entry.AttributesExpiration = time.Now().Add(fs.flags.StatCacheTTL)
	op.Entry.EntryExpiration = time.Now().Add(fs.flags.TypeCacheTTL)

	return
}

func (fs *Goofys) RmDir(
	ctx context.Context,
	op *fuseops.RmDirOp) (err error) {

	fs.mu.Lock()
	parent := fs.getInodeOrDie(op.Parent)
	fs.mu.Unlock()

	err = parent.RmDir(fs, op.Name)
	return
}

func (fs *Goofys) SetInodeAttributes(
	ctx context.Context,
	op *fuseops.SetInodeAttributesOp) (err error) {
	// do nothing, we don't support any of the changes
	return
}

func (fs *Goofys) WriteFile(
	ctx context.Context,
	op *fuseops.WriteFileOp) (err error) {

	fs.mu.Lock()

	fh, ok := fs.fileHandles[op.Handle]
	if !ok {
		panic(fmt.Sprintf("WriteFile: can't find handle %v", op.Handle))
	}
	fs.mu.Unlock()

	err = fh.WriteFile(fs, op.Offset, op.Data)

	return
}

func (fs *Goofys) Unlink(
	ctx context.Context,
	op *fuseops.UnlinkOp) (err error) {

	fs.mu.Lock()
	parent := fs.getInodeOrDie(op.Parent)
	fs.mu.Unlock()

	err = parent.Unlink(fs, op.Name)
	return
}

func (fs *Goofys) Rename(
	ctx context.Context,
	op *fuseops.RenameOp) (err error) {

	fs.mu.Lock()
	parent := fs.getInodeOrDie(op.OldParent)
	newParent := fs.getInodeOrDie(op.NewParent)
	fs.mu.Unlock()

	return parent.Rename(fs, op.OldName, newParent, op.NewName)
}
