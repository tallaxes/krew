// Copyright © 2018 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package download

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/golang/glog"
	"github.com/pkg/errors"
)

// download gets a file from the internet in memory and writes it content
// to a Verifier.
func download(url string, verifier Verifier, fetcher Fetcher) (io.ReaderAt, int64, error) {
	glog.V(2).Infof("Fetching %q", url)
	body, err := fetcher.Get(url)
	if err != nil {
		return nil, 0, errors.Wrapf(err, "could not download %q", url)
	}
	defer body.Close()

	glog.V(3).Infof("Reading download data into memory")
	data, err := ioutil.ReadAll(io.TeeReader(body, verifier))
	if err != nil {
		return nil, 0, errors.Wrap(err, "could not read download content")
	}
	glog.V(2).Infof("Read %d bytes of download data into memory", len(data))

	return bytes.NewReader(data), int64(len(data)), verifier.Verify()
}

// extractZIP extracts a zip file into the target directory.
func extractZIP(targetDir string, read io.ReaderAt, size int64) error {
	glog.V(4).Infof("Extracting download zip to %q", targetDir)
	zipReader, err := zip.NewReader(read, size)
	if err != nil {
		return err
	}

	for _, f := range zipReader.File {
		path := filepath.Join(targetDir, filepath.FromSlash(f.Name))
		if f.FileInfo().IsDir() {
			os.MkdirAll(path, f.Mode())
			continue
		}

		src, err := f.Open()
		if err != nil {
			return errors.Wrap(err, "could not open inflating zip file")
		}

		dst, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode())
		if err != nil {
			return errors.Wrap(err, "can't create file in zip destination dir")
		}

		if _, err := io.Copy(dst, src); err != nil {
			return errors.Wrap(err, "can't copy content to zip destination file")
		}

		// Cleanup the open fd. Don't use defer in case of many files.
		// Don't be blocking
		src.Close()
		dst.Close()
	}

	return nil
}

// extractTARGZ extracts a gzipped tar file into the target directory.
func extractTARGZ(targetDir string, in io.Reader) error {
	glog.V(4).Infof("tar: extracting to %q", targetDir)

	gzr, err := gzip.NewReader(in)
	if err != nil {
		return errors.Wrap(err, "failed to create gzip reader")
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.Wrap(err, "tar extraction error")
		}
		glog.V(4).Infof("tar: processing %q (type=%d, mode=%s)", hdr.Name, hdr.Typeflag, os.FileMode(hdr.Mode))
		// see https://golang.org/cl/78355 for handling pax_global_header
		if hdr.Name == "pax_global_header" {
			glog.V(4).Infof("tar: skipping pax_global_header file")
			continue
		}

		path := filepath.Join(targetDir, filepath.FromSlash(hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, os.FileMode(hdr.Mode)); err != nil {
				return errors.Wrap(err, "failed to create directory from tar")
			}
		case tar.TypeReg:
			dir := filepath.Dir(path)
			glog.V(4).Infof("tar: ensuring parent dirs exist for regular file, dir=%s", dir)
			if err := os.MkdirAll(dir, 0755); err != nil {
				return errors.Wrap(err, "failed to create directory for tar")
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return errors.Wrapf(err, "failed to create file %q", path)
			}
			if _, err := io.Copy(f, tr); err != nil {
				return errors.Wrapf(err, "failed to copy %q from tar into file", hdr.Name)
			}
		default:
			return errors.Errorf("unable to handle file type %d for %q in tar", hdr.Typeflag, hdr.Name)
		}
		glog.V(4).Infof("tar: processed %q", hdr.Name)
	}
	glog.V(4).Infof("tar extraction to %s complete", targetDir)
	return nil
}

func extractArchive(filename, dst string, r io.ReaderAt, size int64) error {
	// TODO(ahmetb) This package is not architected well, this method should not
	// be receiving this many args. Primary problem is at GetInsecure and
	// GetWithSha256 methods that embed extraction in them, which is orthogonal.

	// TODO(ahmetb) write tests with this by mocking extractZIP function into a
	// zipExtractor variable and check its execution.

	if strings.HasSuffix(filename, ".zip") {
		glog.V(4).Infof("detected .zip file")
		return extractZIP(dst, r, size)
	} else if strings.HasSuffix(filename, ".tar.gz") {
		glog.V(4).Infof("detected .tar.gz file")
		return extractTARGZ(dst, io.NewSectionReader(r, 0, size))
	}
	return errors.Errorf("cannot infer a supported archive type from filename in the url (%q)", filename)
}

// Downloader is responsible for fetching, verifying and extracting a binary.
type Downloader struct {
	verifier Verifier
	fetcher  Fetcher
}

// NewDownloader builds a new Downloader.
func NewDownloader(v Verifier, f Fetcher) Downloader {
	return Downloader{
		verifier: v,
		fetcher:  f,
	}
}

// Get pulls the uri and verifies it. On success, the download gets extracted
// into dst.
func (d Downloader) Get(uri, dst string) error {
	body, size, err := download(uri, d.verifier, d.fetcher)
	if err != nil {
		return errors.Wrapf(err, "failed to get the uri %q", uri)
	}
	return extractArchive(path.Base(uri), dst, body, size)
}
