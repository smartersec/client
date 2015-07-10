package main

import (
	"log"
	"os"
	"sync"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/keybase/kbfs/libkbfs"
	"golang.org/x/net/context"
)

func logMsg(msg interface{}) {
	log.Printf("FUSE: %s\n", msg)
}

func runNewFUSE(config libkbfs.Config, debug bool,
	mountpoint string) error {
	if debug {
		fuse.Debug = logMsg
	}

	c, err := fuse.Mount(mountpoint)
	if err != nil {
		return err
	}
	defer c.Close()

	filesys := &FS{
		config: config,
	}
	// TODO: pass in context.WithValue(ctx, ctxAppIDKey, filesys) to Serve
	if err := fs.Serve(c, filesys); err != nil {
		return err
	}

	// check if the mount process has an error to report
	<-c.Ready
	if err := c.MountError; err != nil {
		return err
	}

	return nil
}

// FS implements the newfuse FS interface for KBFS.
type FS struct {
	config libkbfs.Config
}

// context wraps the given Context with a specific value for this FS
// instance.
//
// TODO: get rid of this once fs.Serve() takes a Context.
func (f *FS) context(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxAppIDKey, f)
}

var _ fs.FS = (*FS)(nil)

// Root implements the fs.FS interface for FS.
func (f *FS) Root() (fs.Node, error) {
	n := &Root{
		fs:      f,
		folders: make(map[string]*Dir),
	}
	return n, nil
}

// Root represents the root of the KBFS file system.
type Root struct {
	fs *FS

	mu      sync.Mutex
	folders map[string]*Dir
}

var _ fs.Node = (*Root)(nil)

// Attr implements the fs.Root interface for Root.
func (*Root) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0755
	return nil
}

var _ fs.NodeRequestLookuper = (*Root)(nil)

// getMD is a wrapper over KBFSOps.GetOrCreateRootNodeForHandle that gives
// useful results for home folders with public subdirectories.
func (r *Root) getMD(ctx context.Context, dh *libkbfs.TlfHandle) (libkbfs.Node, error) {
	rootNode, _, err :=
		r.fs.config.KBFSOps().
			GetOrCreateRootNodeForHandle(ctx, dh, libkbfs.MasterBranch)
	if err != nil {
		if _, ok := err.(libkbfs.ReadAccessError); ok && dh.HasPublic() {
			// This user cannot get the metadata for the folder, but
			// we know it has a public subdirectory, so serve it
			// anyway.
			return nil, nil
		}
		return nil, err
	}

	return rootNode, nil
}

// Lookup implements the fs.NodeRequestLookuper interface for Root.
func (r *Root) Lookup(ctx context.Context, req *fuse.LookupRequest, resp *fuse.LookupResponse) (fs.Node, error) {
	ctx = r.fs.context(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()

	if child, ok := r.folders[req.Name]; ok {
		return child, nil
	}

	dh, err := libkbfs.ParseTlfHandle(ctx, r.fs.config, req.Name)
	if err != nil {
		return nil, err
	}
	if dh.IsPublic() {
		// public directories shouldn't be listed directly in root
		return nil, fuse.ENOENT
	}

	if canon := dh.ToString(r.fs.config); canon != req.Name {
		n := &Alias{
			canon: canon,
		}
		return n, nil
	}

	rootNode, err := r.getMD(ctx, dh)
	if err != nil {
		// TODO make errors aware of fuse
		return nil, err
	}

	mdID := libkbfs.NullTlfID
	if rootNode != nil {
		mdID = rootNode.GetFolderBranch().Tlf
	}
	folder := &Folder{
		fs: r.fs,
		id: mdID,
		dh: dh,
	}
	child := &Dir{
		folder: folder,
		node:   rootNode,
	}
	r.folders[req.Name] = child
	return child, nil
}

var _ fs.Handle = (*Root)(nil)

var _ fs.HandleReadDirAller = (*Root)(nil)

func (r *Root) getDirent(ctx context.Context, work <-chan libkbfs.TlfID, results chan<- fuse.Dirent) error {
	for {
		select {
		case tlfID, ok := <-work:
			if !ok {
				return nil
			}
			_, _, dh, err := r.fs.config.KBFSOps().GetRootNode(
				ctx, libkbfs.FolderBranch{tlfID, libkbfs.MasterBranch})
			if err != nil {
				return err
			}
			name := dh.ToString(r.fs.config)
			results <- fuse.Dirent{
				Type: fuse.DT_Dir,
				Name: name,
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ReadDirAll implements the ReadDirAll interface for Root.
func (r *Root) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	ctx = r.fs.context(ctx)
	favs, err := r.fs.config.KBFSOps().GetFavDirs(ctx)
	if err != nil {
		return nil, err
	}
	work := make(chan libkbfs.TlfID)
	results := make(chan fuse.Dirent)
	errCh := make(chan error, 1)
	const workers = 10
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.getDirent(ctx, work, results); err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}()
	}

	go func() {
		// feed work
		for _, tlfID := range favs {
			work <- tlfID
		}
		close(work)
		wg.Wait()
		// workers are done
		close(results)
	}()

	var res []fuse.Dirent
outer:
	for {
		select {
		case dirent, ok := <-results:
			if !ok {
				break outer
			}
			res = append(res, dirent)
		case err := <-errCh:
			return nil, err
		}
	}
	return res, nil
}
