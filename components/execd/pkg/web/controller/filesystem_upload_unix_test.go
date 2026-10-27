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

//go:build !windows

package controller

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A new destination must land with the umask-filtered creation mode, exactly
// as the old O_CREATE open produced it - not with the raw staging mode that
// an explicit chmod would set.
func TestWriteUploadStreamAppliesUmaskToNewDestinations(t *testing.T) {
	prev := syscall.Umask(0o022)
	defer syscall.Umask(prev)

	dir := t.TempDir()
	dest := filepath.Join(dir, "fresh.md")

	if uerr := writeUploadStream(dest, strings.NewReader("contents\n")); uerr != nil {
		t.Fatalf("unexpected upload error: %v", uerr.message)
	}

	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Fatalf("new destination mode: want 0755 (0777 through umask 022), got %o", got)
	}
}

// A writable destination under an unwritable parent directory always worked
// with the in-place write; staging is impossible there, so the upload keeps
// the historical semantics instead of failing.
func TestWriteUploadStreamFallsBackInPlaceWhenParentUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write bits, so the fallback path cannot be reached")
	}

	dir := t.TempDir()
	dest := filepath.Join(dir, "shared.md")
	if err := os.WriteFile(dest, []byte("old contents\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if uerr := writeUploadStream(dest, strings.NewReader("new contents\n")); uerr != nil {
		t.Fatalf("upload that always worked now fails: %v", uerr.message)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new contents\n" {
		t.Fatalf("want %q, got %q", "new contents\n", string(got))
	}
}
