package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/internal/squashfs"
	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
	"golang.org/x/exp/mmap"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

const installHelp = `TODO
`

// totalBytes counts the number of bytes written to the disk for this install
// operation.
var totalBytes int64

type errNotFound struct{}

func (errNotFound) Error() string {
	return "HTTP status 404"
}

func isNotExist(err error) bool {
	if _, ok := err.(*errNotFound); ok {
		return true
	}
	return os.IsNotExist(err)
}

func repoReader(repo distri.Repo, fn string) (io.ReadCloser, error) {
	if strings.HasPrefix(repo.Path, "http://") ||
		strings.HasPrefix(repo.Path, "https://") {
		req, err := http.NewRequest("GET", repo.Path+"/"+fn, nil) // TODO: sanitize slashes
		if err != nil {
			return nil, err
		}
		if os.Getenv("DISTRI_REEXEC") == "1" {
			req.Header.Set("X-Distri-Reexec", "yes")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		if got, want := resp.StatusCode, http.StatusOK; got != want {
			if got == http.StatusNotFound {
				return nil, &errNotFound{}
			}
			return nil, fmt.Errorf("HTTP status %v", resp.Status)
		}
		return resp.Body, nil
	}
	return os.Open(filepath.Join(repo.Path, fn))
}

func unpackDir(dest string, rd *squashfs.Reader, inode squashfs.Inode) error {
	if err := os.MkdirAll(dest, 0755); err != nil {
		return err
	}
	fis, err := rd.Readdir(inode)
	if err != nil {
		return err
	}
	for _, fi := range fis {
		destName := filepath.Join(dest, fi.Name())
		fileInode := fi.Sys().(*squashfs.FileInfo).Inode
		if fi.IsDir() {
			if err := unpackDir(destName, rd, fileInode); err != nil {
				return err
			}
		} else if fi.Mode()&os.ModeSymlink > 0 {
			target, err := rd.ReadLink(fileInode)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, destName); err != nil {
				// TODO: if exists, re-create if target differs
				return err
			}
		} else if fi.Mode().IsRegular() {
			fr, err := rd.FileReader(fileInode)
			if err != nil {
				return err
			}
			f, err := os.OpenFile(destName, os.O_RDWR|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
			if err != nil {
				return err
			}
			n, err := io.Copy(f, fr)
			if err != nil {
				return err
			}
			atomic.AddInt64(&totalBytes, n)
			if err := f.Close(); err != nil {
				return err
			}
		} else {
			log.Printf("ERROR: unsupported SquashFS file type: %+v", fi.Mode())
		}
	}
	return nil
}

func install1(root string, repo distri.Repo, pkg string, first bool) error {
	if _, err := os.Stat(filepath.Join(root, "roimg", pkg+".squashfs")); err == nil {
		return nil // package already installed
	}

	tmpDir := filepath.Join(root, "roimg", "tmp", "."+pkg+fmt.Sprintf("%d", os.Getpid()))
	if err := os.Mkdir(tmpDir, 0755); err != nil {
		if os.IsExist(err) {
			return nil // another goroutine is installing this package
		}
		return err
	}

	log.Printf("installing package %q to root %s", pkg, root)

	for _, fn := range []string{pkg + ".squashfs", pkg + ".meta.textproto"} {
		f, err := os.Create(filepath.Join(tmpDir, fn))
		if err != nil {
			return err
		}
		in, err := repoReader(repo, "pkg/"+fn)
		if err != nil {
			return err
		}
		defer in.Close()
		n, err := io.Copy(f, in)
		if err != nil {
			return err
		}
		atomic.AddInt64(&totalBytes, n)
		in.Close()
		if err := f.Close(); err != nil {
			return err
		}
	}

	// first is true only on the first installation of the package (regardless
	// of its version).
	if first {
		readerAt, err := mmap.Open(filepath.Join(tmpDir, pkg+".squashfs"))
		if err != nil {
			return fmt.Errorf("copying /etc: %v", err)
		}

		rd, err := squashfs.NewReader(readerAt)
		if err != nil {
			return err
		}

		fis, err := rd.Readdir(rd.RootInode())
		if err != nil {
			return err
		}
		for _, fi := range fis {
			if fi.Name() != "etc" {
				continue
			}
			log.Printf("copying %s/etc", pkg)
			if err := unpackDir(filepath.Join(root, "etc"), rd, fi.Sys().(*squashfs.FileInfo).Inode); err != nil {
				return fmt.Errorf("copying /etc: %v", err)
			}
			break
		}
	}

	// First meta, then image: the fuse daemon considers the image canonical, so
	// it must go last.
	for _, fn := range []string{pkg + ".meta.textproto", pkg + ".squashfs"} {
		if err := os.Rename(filepath.Join(tmpDir, fn), filepath.Join(root, "roimg", fn)); err != nil {
			return err
		}
	}

	if err := os.Remove(tmpDir); err != nil {
		return err
	}

	return nil
}

func installTransitively1(root string, repos []distri.Repo, pkg string) error {
	origpkg := pkg
	if _, ok := hasArchSuffix(pkg); !ok && !likelyFullySpecified(pkg) {
		pkg += "-amd64" // TODO: configurable / auto-detect
	}
	metas := make(map[*pb.Meta]distri.Repo)
	for _, repo := range repos {
		rd, err := repoReader(repo, "pkg/"+pkg+".meta.textproto")
		if err != nil {
			if isNotExist(err) {
				continue
			}
			return err
		}
		b, err := ioutil.ReadAll(rd)
		rd.Close()
		if err != nil {
			return err
		}
		var pm pb.Meta
		if err := proto.UnmarshalText(string(b), &pm); err != nil {
			return err
		}
		metas[&pm] = repo
	}
	var pm *pb.Meta
	var repo distri.Repo
	for m, r := range metas {
		if pm == nil || m.GetVersion() > pm.GetVersion() {
			pm = m
			repo = r
		}
	}
	if pm == nil {
		return fmt.Errorf("package %s not found on any configured repo", pkg)
	}

	if _, ok := hasArchSuffix(pkg); ok {
		pkg += "-" + pm.GetVersion()
	}

	// TODO(later): we could write out b here and save 1 HTTP request
	pkgs := append([]string{pkg}, pm.GetRuntimeDep()...)
	log.Printf("resolved %s to %v", origpkg, pkgs)

	// TODO: figure out if this is the first installation by checking existence
	// in the corresponding pkgset file
	first := true

	// download all packages with maximum concurrency for the time being
	var eg errgroup.Group
	for _, pkg := range pkgs {
		pkg := pkg //copy
		eg.Go(func() error {
			if err := install1(root, repo, pkg, first); err != nil {
				return fmt.Errorf("installing %s: %v", pkg, err)
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}

	// TODO(later): tell the FUSE daemon that a (single) new package is
	// available so that new packages can be used while a bunch of them are
	// being installed?

	return nil
}

func install(args []string) error {
	fset := flag.NewFlagSet("install", flag.ExitOnError)
	var (
		root = fset.String("root",
			"/",
			"root directory for optionally installing into a chroot")

		repo = fset.String("repo", "", "repository from which to install packages from. path (default TODO) or HTTP URL (e.g. TODO)")

		//pkg = fset.String("pkg", "", "path to .squashfs package to mount")
	)
	fset.Parse(args)
	if fset.NArg() < 1 {
		return fmt.Errorf("syntax: install [options] <package> [<package>...]")
	}

	repos, err := env.Repos()
	if err != nil {
		return err
	}
	if *repo != "" {
		repos = []distri.Repo{{Path: *repo}}
	}
	if len(repos) == 0 {
		return fmt.Errorf("no repos configured")
	}

	// TODO: lock to ensure only one process modifies roimg at a time

	tmpDir := filepath.Join(*root, "roimg", "tmp")

	// Remove stale work directories of previously interrupted/crashed processes.
	if err := os.RemoveAll(tmpDir); err != nil {
		return err
	}

	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return err
	}

	start := time.Now()

	var eg errgroup.Group
	for _, pkg := range fset.Args() {
		pkg := pkg // copy
		eg.Go(func() error { return installTransitively1(*root, repos, pkg) })
	}
	if err := eg.Wait(); err != nil {
		return err
	}

	// Make the FUSE daemon update its packages.
	ctl, err := os.Readlink(filepath.Join(*root, "ro", "ctl"))
	if err != nil {
		log.Printf("not updating FUSE daemon: %v", err)
		return nil // no FUSE daemon running?
	}

	log.Printf("connecting to %s", ctl)
	ctx := context.Background()
	conn, err := grpc.DialContext(ctx, "unix://"+ctl, grpc.WithBlock(), grpc.WithInsecure())
	if err != nil {
		return err
	}
	cl := pb.NewFUSEClient(conn)
	if _, err := cl.ScanPackages(ctx, &pb.ScanPackagesRequest{}); err != nil {
		return err
	}

	dur := time.Since(start)

	total := atomic.LoadInt64(&totalBytes)
	log.Printf("done, %.2f MB/s (%v bytes in %v)", float64(total)/1024/1024/(float64(dur)/float64(time.Second)), total, dur)

	return nil
}
