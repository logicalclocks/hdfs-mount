// Copyright (c) Microsoft. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for details.
package main

import (
	"io"
	"sync"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

// Represends a handle to an open file
type FileHandle struct {
	File              *FileINode
	Mutex             sync.Mutex     // all operations on the handle are serialized to simplify invariants
	fileFlags         fuse.OpenFlags // flags used to creat the file
	tatalBytesRead    int64
	totalBytesWritten int64
	fhID              int64 // file handle id. for debugging only
}

// Verify that *FileHandle implements necesary FUSE interfaces
var _ fs.Node = (*FileHandle)(nil)
var _ fs.HandleReader = (*FileHandle)(nil)
var _ fs.HandleReleaser = (*FileHandle)(nil)
var _ fs.HandleWriter = (*FileHandle)(nil)
var _ fs.NodeFsyncer = (*FileHandle)(nil)
var _ fs.HandleFlusher = (*FileHandle)(nil)

func (fh *FileHandle) dataChanged() bool {
	if fh.totalBytesWritten > 0 {
		return true
	} else {
		return false
	}
}

func (fh *FileHandle) Truncate(size int64) error {
	err := fh.File.handle.Truncate(size)
	if err != nil {
		logerror("Failed to truncate file", fh.logInfo(Fields{Operation: Truncate, Bytes: size, Error: err}))
	}
	loginfo("Truncated file", fh.logInfo(Fields{Operation: Truncate, Bytes: size}))
	return nil
}

// Returns attributes of the file associated with this handle
func (fh *FileHandle) Attr(ctx context.Context, a *fuse.Attr) error {
	fh.Mutex.Lock()
	defer fh.Mutex.Unlock()
	return fh.File.Attr(ctx, a)
}

// Responds to FUSE Read request
func (fh *FileHandle) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	fh.Mutex.Lock()
	defer fh.Mutex.Unlock()

	buf := resp.Data[0:req.Size]
	nr, err := fh.File.handle.ReadAt(buf, req.Offset)
	resp.Data = buf[0:nr]
	fh.tatalBytesRead += int64(nr)

	if err != nil {
		if err == io.EOF {
			// EOF isn't a error, reporting successful read to FUSE
			logdebug("Completed reading", fh.logInfo(Fields{Operation: Read, Error: err, Bytes: nr}))
			return nil
		} else {
			logerror("Failed to read", fh.logInfo(Fields{Operation: Read, Error: err, Bytes: nr}))
			return err
		}
	}
	return err
}

// Responds to FUSE Write request
func (fh *FileHandle) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	fh.Mutex.Lock()
	defer fh.Mutex.Unlock()

	// as an optimization the file is initially opened in readonly mode
	fh.File.upgradeHandleForWriting()

	nw, err := fh.File.handle.WriteAt(req.Data, req.Offset)
	resp.Size = nw
	fh.totalBytesWritten += int64(nw)
	if err != nil {
		logerror("Failed to write to staging file", fh.logInfo(Fields{Operation: Write, Error: err}))
		return err
	} else {
		logdebug("Write data to staging file", fh.logInfo(Fields{Operation: Write, Bytes: nw}))
		return nil
	}
}

func (fh *FileHandle) copyToDFS(operation string) error {
	if fh.totalBytesWritten == 0 { // Nothing to do
		return nil
	}
	defer fh.File.InvalidateMetadataCache()

	logdebug("Uploading to DFS", fh.logInfo(Fields{Operation: Write, Bytes: TotalBytesWritten}))

	op := fh.File.FileSystem.RetryPolicy.StartOperation()
	for {
		err := fh.FlushAttempt(operation)
		if err != io.EOF || IsSuccessOrNonRetriableError(err) || !op.ShouldRetry("Flush() %s", err) {
			return err
		}
		// Reconnect and try again
		fh.File.FileSystem.getDFSConnector().Close()
		logwarn("Failed to copy file to DFS", fh.logInfo(Fields{Operation: operation}))
	}
}

func (fh *FileHandle) FlushAttempt(operation string) error {
	hdfsAccessor := fh.File.FileSystem.getDFSConnector()
	w, err := hdfsAccessor.CreateFile(fh.File.AbsolutePath(), fh.File.Attrs.Mode, true)
	if err != nil {
		logerror("Error creating file in DFS", fh.logInfo(Fields{Operation: operation, Error: err}))
		return err
	}

	//open the file for reading and upload to DFS
	err = fh.File.handle.SeekToStart()
	if err != nil {
		logerror("Unable to seek to the begenning of the temp file", fh.logInfo(Fields{Operation: operation, Error: err}))
		return err
	}

	b := make([]byte, 65536)
	written := 0
	for {
		nr, err := fh.File.handle.Read(b)
		if err != nil {
			if err != io.EOF {
				logerror("Failed to read from staging file", fh.logInfo(Fields{Operation: operation, Error: err}))
			}
			break
		}
		b = b[:nr]

		nw, err := w.Write(b)
		if err != nil {
			logerror("Failed to write to DFS", fh.logInfo(Fields{Operation: operation, Error: err}))
			w.Close()
			return err
		}
		logtrace("Written to DFS", fh.logInfo(Fields{Operation: operation, Bytes: nw}))
		written += nw
	}

	err = w.Close()
	if err != nil {
		logerror("Failed to close file in DFS", fh.logInfo(Fields{Operation: operation, Error: err}))
		return err
	}
	loginfo("Uploaded to DFS", fh.logInfo(Fields{Operation: operation, Bytes: written}))
	return nil
}

// Responds to the FUSE Flush request
func (fh *FileHandle) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	fh.Mutex.Lock()
	defer fh.Mutex.Unlock()
	if fh.dataChanged() {
		loginfo("Flush file", fh.logInfo(Fields{Operation: Flush}))
		return fh.copyToDFS(Flush)
	} else {
		return nil
	}
}

// Responds to the FUSE Fsync request
func (fh *FileHandle) Fsync(ctx context.Context, req *fuse.FsyncRequest) error {
	fh.Mutex.Lock()
	defer fh.Mutex.Unlock()
	if fh.dataChanged() {
		loginfo("Fsync file", fh.logInfo(Fields{Operation: Fsync}))
		return fh.copyToDFS(Fsync)
	} else {
		return nil
	}
}

// Closes the handle
func (fh *FileHandle) Release(_ context.Context, _ *fuse.ReleaseRequest) error {
	fh.Mutex.Lock()
	defer fh.Mutex.Unlock()

	//close the file handle if it is the last handle
	fh.File.InvalidateMetadataCache()
	fh.File.RemoveHandle(fh)
	activeHandles := fh.File.countActiveHandles()
	if activeHandles == 0 {
		err := fh.File.handle.Close()
		if err != nil {
			logerror("Failed to close staging file", fh.logInfo(Fields{Operation: Close, Error: err}))
		}
		fh.File.handle = nil
		loginfo("Staging file is closed", fh.logInfo(Fields{Operation: Close, Flags: fh.fileFlags, TotalBytesRead: fh.tatalBytesRead, TotalBytesWritten: fh.totalBytesWritten}))
	} else {
		logdebug("Staging file is not closed becuase it has other active handles ", fh.logInfo(Fields{Operation: Close, Flags: fh.fileFlags, TotalBytesRead: fh.tatalBytesRead, TotalBytesWritten: fh.totalBytesWritten}))
	}

	loginfo("Close file", fh.logInfo(Fields{Operation: Close, Flags: fh.fileFlags, TotalBytesRead: fh.tatalBytesRead, TotalBytesWritten: fh.totalBytesWritten}))
	return nil
}

func (fh *FileHandle) logInfo(fields Fields) Fields {
	f := Fields{FileHandleID: fh.fhID, Path: fh.File.AbsolutePath()}
	for k, e := range fields {
		f[k] = e
	}
	return f
}
