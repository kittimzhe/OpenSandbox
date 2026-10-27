// Copyright 2025 The OpenSandbox Authors
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

package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

type uploadError struct {
	status  int
	code    model.ErrorCode
	message string
}

func newUploadError(status int, code model.ErrorCode, message string) *uploadError {
	return &uploadError{status: status, code: code, message: message}
}

func (c *FilesystemController) UploadFile() {
	rec := beginFilesystemMetric("upload")
	defer rec.Finish(c.basicController)

	metadataParts, fileParts, uerr := c.parseUploadForm()
	if uerr != nil {
		c.RespondError(uerr.status, uerr.code, uerr.message)
		return
	}

	for i := range metadataParts {
		if uerr := c.processUploadPair(metadataParts[i], fileParts[i]); uerr != nil {
			c.RespondError(uerr.status, uerr.code, uerr.message)
			return
		}
	}

	rec.MarkSuccess()
	c.RespondSuccess(nil)
}

func (c *FilesystemController) parseUploadForm() ([]*multipart.FileHeader, []*multipart.FileHeader, *uploadError) {
	return parseUploadForm(c.ctx)
}

func parseUploadForm(ctx *gin.Context) ([]*multipart.FileHeader, []*multipart.FileHeader, *uploadError) {
	form, err := ctx.MultipartForm()
	if err != nil || form == nil {
		return nil, nil, newUploadError(http.StatusBadRequest, model.ErrorCodeInvalidFile, "multipart form is empty")
	}

	metadataParts := form.File["metadata"]
	fileParts := form.File["file"]

	if len(metadataParts) == 0 {
		return nil, nil, newUploadError(http.StatusBadRequest, model.ErrorCodeInvalidFileMetadata, "metadata file is missing")
	}
	if len(fileParts) == 0 {
		return nil, nil, newUploadError(http.StatusBadRequest, model.ErrorCodeInvalidFileContent, "file is missing")
	}
	if len(metadataParts) != len(fileParts) {
		return nil, nil, newUploadError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidFile,
			fmt.Sprintf("metadata and file count mismatch: %d vs %d", len(metadataParts), len(fileParts)),
		)
	}
	return metadataParts, fileParts, nil
}

func (c *FilesystemController) processUploadPair(metadataHeader, fileHeader *multipart.FileHeader) *uploadError {
	meta, uerr := parseUploadMetadata(metadataHeader)
	if uerr != nil {
		return uerr
	}

	resolvedPath, uerr := resolveUploadTarget(meta.Path, meta.Permission)
	if uerr != nil {
		return uerr
	}

	if uerr := writeUploadFile(resolvedPath, fileHeader); uerr != nil {
		return uerr
	}

	return applyUploadPermission(resolvedPath, meta.Permission)
}

func parseUploadMetadata(header *multipart.FileHeader) (*model.FileMetadata, *uploadError) {
	metadataFile, err := header.Open()
	if err != nil {
		return nil, newUploadError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidFileMetadata,
			fmt.Sprintf("error opening metadata file. %v", err),
		)
	}
	metaBytes, err := io.ReadAll(metadataFile)
	metadataFile.Close()
	if err != nil {
		return nil, newUploadError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidFileMetadata,
			fmt.Sprintf("error reading metadata content. %v", err),
		)
	}

	var meta model.FileMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, newUploadError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidFileMetadata,
			fmt.Sprintf("invalid metadata format. %v", err),
		)
	}
	if meta.Path == "" {
		return nil, newUploadError(http.StatusBadRequest, model.ErrorCodeInvalidFileMetadata, "metadata path is empty")
	}
	return &meta, nil
}

func resolveUploadTarget(targetPath string, perm model.Permission) (string, *uploadError) {
	resolvedPath, err := pathutil.ExpandPath(targetPath)
	if err != nil {
		return "", newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error resolving target path %s. %v", targetPath, err),
		)
	}
	targetDir := filepath.Dir(resolvedPath)
	if err := MkdirAllWithOwnership(targetDir, os.ModePerm, perm.Owner, perm.Group); err != nil {
		return "", newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error creating target directory %s. %v", targetDir, err),
		)
	}
	return resolvedPath, nil
}

func writeUploadFile(resolvedPath string, fileHeader *multipart.FileHeader) *uploadError {
	file, err := fileHeader.Open()
	if err != nil {
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error opening file %s. %v", fileHeader.Filename, err),
		)
	}
	defer file.Close()

	return writeUploadStream(resolvedPath, file)
}

// writeUploadStream stores an upload atomically: the bytes are staged in a
// temp file inside the destination directory and renamed over the destination
// only after every byte is written and synced. Any failure before that point -
// a broken request stream, ENOSPC, an I/O error, execd dying mid-copy - leaves
// the destination exactly as it was. The previous open(O_TRUNC)-then-copy
// design truncated the destination up front, turning a failed upload into
// unrecoverable data loss (#2432).
func writeUploadStream(resolvedPath string, src io.Reader) *uploadError {
	dest := followUploadSymlink(resolvedPath)

	// Reject what the in-place write rejected (a directory, a special file,
	// a destination this process cannot open for writing) before staging
	// anything: a rename would otherwise happily replace a destination the
	// old code could not even open. The probe carries no O_TRUNC, so it
	// cannot damage the destination.
	if fi, err := os.Lstat(dest); err == nil && !fi.Mode().IsRegular() {
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("destination %s is not a regular file", resolvedPath),
		)
	}
	probe, err := os.OpenFile(dest, os.O_WRONLY, 0)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error opening destination file %s. %v", resolvedPath, err),
		)
	}
	if err == nil {
		probe.Close()
	}

	// Keep what an in-place write preserved for free: the destination's
	// mode, and on Unix its owner and group. New destinations are staged
	// with the same mode the old O_CREATE used.
	mode := os.ModePerm
	uid, gid := -1, -1
	if fi, err := os.Stat(dest); err == nil {
		mode = fi.Mode().Perm()
		uid, gid = uploadOwnerOf(fi)
	}

	targetDir := filepath.Dir(dest)
	staged, err := os.CreateTemp(targetDir, ".upload-*.tmp")
	if err != nil {
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error creating staging file for %s. %v", resolvedPath, err),
		)
	}
	stagedName := staged.Name()
	defer os.Remove(stagedName) // no-op once renamed into place

	if err := os.Chmod(stagedName, mode); err != nil {
		staged.Close()
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error chmoding staging file for %s. %v", resolvedPath, err),
		)
	}
	if uid >= 0 || gid >= 0 {
		// Best-effort like the directory sync below: a non-root execd
		// cannot chown, and the metadata permission pass after the rename
		// re-asserts ownership from the request anyway.
		if err := os.Chown(stagedName, uid, gid); err != nil {
			log.Warn("upload: chown staging file %s: %v", stagedName, err)
		}
	}

	if _, err := io.Copy(staged, src); err != nil {
		staged.Close()
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error copying file %s. %v", resolvedPath, err),
		)
	}
	if err := staged.Sync(); err != nil {
		staged.Close()
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error syncing staging file for %s. %v", resolvedPath, err),
		)
	}
	if err := staged.Close(); err != nil {
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error closing staging file for %s. %v", resolvedPath, err),
		)
	}
	if err := os.Rename(stagedName, dest); err != nil {
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error replacing destination file %s. %v", resolvedPath, err),
		)
	}

	// fsync parent directory so the rename is durable and visible on
	// weakly-coherent filesystems (virtio-fs, 9pfs, etc.). Best-effort:
	// some filesystems return ENOTSUP for directory fsync.
	if d, err := os.Open(targetDir); err == nil {
		if err := d.Sync(); err != nil {
			log.Warn("upload: sync parent dir %s: %v", targetDir, err)
		}
		_ = d.Close()
	}
	return nil
}

// followUploadSymlink resolves a symlink in the final path component the way
// the previous in-place open did: an upload through `link -> real` must
// replace `real`, not the link itself, and a dangling link must create its
// (still missing) target rather than overwriting the link. Symlinks in parent
// components are followed by the kernel for open and rename alike, so only
// the final component is walked here.
func followUploadSymlink(path string) string {
	for i := 0; i < 40; i++ {
		fi, err := os.Lstat(path)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			return path
		}
		dest, err := os.Readlink(path)
		if err != nil {
			return path
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(filepath.Dir(path), dest)
		}
		path = dest
	}
	return path
}

// applyUploadPermission applies the metadata permission with one retry to
// absorb metadata-propagation delay on weakly-coherent filesystems
// (virtio-fs, 9pfs). ChmodFile always invokes chown under the hood, so a
// freshly-created dirent that has not yet propagated will surface as ENOENT
// here even though the file is fully written and synced.
func applyUploadPermission(resolvedPath string, permission model.Permission) *uploadError {
	chmodErr := ChmodFile(resolvedPath, permission)
	if chmodErr != nil {
		time.Sleep(20 * time.Millisecond)
		chmodErr = ChmodFile(resolvedPath, permission)
	}
	if chmodErr != nil {
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error chmoding file %s. %v", resolvedPath, chmodErr),
		)
	}
	return nil
}
