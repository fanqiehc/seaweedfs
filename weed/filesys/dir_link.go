package filesys

import (
	"context"
	"github.com/chrislusf/seaweedfs/weed/util"
	"os"
	"syscall"
	"time"

	"github.com/chrislusf/seaweedfs/weed/filer"
	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/fuse"
	"github.com/seaweedfs/fuse/fs"
)

var _ = fs.NodeLinker(&Dir{})
var _ = fs.NodeSymlinker(&Dir{})
var _ = fs.NodeReadlinker(&File{})

const (
	HARD_LINK_MARKER = '\x01'
)

func (dir *Dir) Link(ctx context.Context, req *fuse.LinkRequest, old fs.Node) (fs.Node, error) {

	if dir.wfs.option.ReadOnly {
		return nil, fuse.EPERM
	}

	oldFile, ok := old.(*File)
	if !ok {
		glog.Errorf("old node is not a file: %+v", old)
	}

	glog.V(4).Infof("Link: %v/%v -> %v/%v", oldFile.dir.FullPath(), oldFile.Name, dir.FullPath(), req.NewName)

	if _, err := oldFile.maybeLoadEntry(ctx); err != nil {
		return nil, err
	}

	oldEntry := oldFile.getEntry()
	if oldEntry == nil {
		return nil, fuse.EIO
	}

	// update old file to hardlink mode
	if len(oldEntry.HardLinkId) == 0 {
		oldEntry.HardLinkId = append(util.RandomBytes(16), HARD_LINK_MARKER)
		oldEntry.HardLinkCounter = 1
	}
	oldEntry.HardLinkCounter++
	updateOldEntryRequest := &filer_pb.UpdateEntryRequest{
		Directory:  oldFile.dir.FullPath(),
		Entry:      oldEntry,
		Signatures: []int32{dir.wfs.signature},
	}

	// CreateLink 1.2 : update new file to hardlink mode
	request := &filer_pb.CreateEntryRequest{
		Directory: dir.FullPath(),
		Entry: &filer_pb.Entry{
			Name:            req.NewName,
			IsDirectory:     false,
			Attributes:      oldEntry.Attributes,
			Chunks:          oldEntry.Chunks,
			Extended:        oldEntry.Extended,
			HardLinkId:      oldEntry.HardLinkId,
			HardLinkCounter: oldEntry.HardLinkCounter,
		},
		Signatures: []int32{dir.wfs.signature},
	}

	// apply changes to the filer, and also apply to local metaCache
	err := dir.wfs.WithFilerClient(func(client filer_pb.SeaweedFilerClient) error {

		dir.wfs.mapPbIdFromLocalToFiler(request.Entry)
		defer dir.wfs.mapPbIdFromFilerToLocal(request.Entry)

		if err := filer_pb.UpdateEntry(client, updateOldEntryRequest); err != nil {
			glog.V(0).Infof("Link %v/%v -> %s/%s: %v", oldFile.dir.FullPath(), oldFile.Name, dir.FullPath(), req.NewName, err)
			return fuse.EIO
		}
		dir.wfs.metaCache.UpdateEntry(context.Background(), filer.FromPbEntry(updateOldEntryRequest.Directory, updateOldEntryRequest.Entry))

		if err := filer_pb.CreateEntry(client, request); err != nil {
			glog.V(0).Infof("Link %v/%v -> %s/%s: %v", oldFile.dir.FullPath(), oldFile.Name, dir.FullPath(), req.NewName, err)
			return fuse.EIO
		}
		dir.wfs.metaCache.InsertEntry(context.Background(), filer.FromPbEntry(request.Directory, request.Entry))

		return nil
	})

	if err != nil {
		return nil, fuse.EIO
	}

	// create new file node
	newNode := dir.newFile(req.NewName, request.Entry)
	newFile := newNode.(*File)
	if _, err := newFile.maybeLoadEntry(ctx); err != nil {
		return nil, err
	}

	return newFile, err

}

func (dir *Dir) Symlink(ctx context.Context, req *fuse.SymlinkRequest) (fs.Node, error) {

	if dir.wfs.option.ReadOnly {
		return nil, fuse.EPERM
	}

	glog.V(4).Infof("Symlink: %v/%v to %v", dir.FullPath(), req.NewName, req.Target)

	request := &filer_pb.CreateEntryRequest{
		Directory: dir.FullPath(),
		Entry: &filer_pb.Entry{
			Name:        req.NewName,
			IsDirectory: false,
			Attributes: &filer_pb.FuseAttributes{
				Mtime:         time.Now().Unix(),
				Crtime:        time.Now().Unix(),
				FileMode:      uint32((os.FileMode(0777) | os.ModeSymlink) &^ dir.wfs.option.Umask),
				Uid:           req.Uid,
				Gid:           req.Gid,
				SymlinkTarget: req.Target,
			},
		},
		Signatures: []int32{dir.wfs.signature},
	}

	err := dir.wfs.WithFilerClient(func(client filer_pb.SeaweedFilerClient) error {

		dir.wfs.mapPbIdFromLocalToFiler(request.Entry)
		defer dir.wfs.mapPbIdFromFilerToLocal(request.Entry)

		if err := filer_pb.CreateEntry(client, request); err != nil {
			glog.V(0).Infof("symlink %s/%s: %v", dir.FullPath(), req.NewName, err)
			return fuse.EIO
		}

		dir.wfs.metaCache.InsertEntry(context.Background(), filer.FromPbEntry(request.Directory, request.Entry))

		return nil
	})

	symlink := dir.newFile(req.NewName, request.Entry)

	return symlink, err

}

func (file *File) Readlink(ctx context.Context, req *fuse.ReadlinkRequest) (string, error) {

	entry, err := file.maybeLoadEntry(ctx)
	if err != nil {
		return "", err
	}

	if os.FileMode(entry.Attributes.FileMode)&os.ModeSymlink == 0 {
		return "", fuse.Errno(syscall.EINVAL)
	}

	glog.V(4).Infof("Readlink: %v/%v => %v", file.dir.FullPath(), file.Name, entry.Attributes.SymlinkTarget)

	return entry.Attributes.SymlinkTarget, nil

}
