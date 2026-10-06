package e2b

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	"github.com/jrimmer/spoond/v2/substrate"
	"github.com/jrimmer/spoond/v2/substrate/e2b/gen/envd/filesystem"
	"github.com/jrimmer/spoond/v2/substrate/e2b/gen/envd/filesystem/filesystemconnect"
)

// WriteFile writes data to path via envd's HTTP POST /files (multipart).
// envd sets no mode, so the file is first created empty with the requested
// mode (and any missing parents, 0755) by an exec install; the upload then
// fills it, and a chmod afterwards re-applies the mode in case the upload
// replaced the file rather than truncating it.
func (c *Client) WriteFile(ctx context.Context, sandboxID, path string, data []byte, mode os.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	if err := c.precreate(ctx, sandboxID, path, mode); err != nil {
		return err
	}
	if err := uploadFile(ctx, c.envdClient(sandboxID, ""), c.cfg.ProxyURL, path, data); err != nil {
		if !c.listed(ctx, sandboxID) {
			return fmt.Errorf("%w: %v", substrate.ErrNotFound, err)
		}
		return err
	}
	return c.chmod(ctx, sandboxID, path, mode)
}

// ReadFile downloads path via envd's HTTP GET /files; the error wraps
// substrate.ErrNotFound for a missing path and substrate.ErrTooLarge when the
// file is larger than max bytes.
func (c *Client) ReadFile(ctx context.Context, sandboxID, path string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, filesURL(c.cfg.ProxyURL, path), nil)
	if err != nil {
		return nil, fmt.Errorf("e2b: read %s %s: %w", sandboxID, path, err)
	}
	resp, err := c.envdClient(sandboxID, "").Do(req)
	if err != nil {
		return nil, fmt.Errorf("e2b: read %s %s: %w", sandboxID, path, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		// read below
	case http.StatusNotFound:
		return nil, fmt.Errorf("e2b: read %s %s: %w", sandboxID, path, substrate.ErrNotFound)
	default:
		return nil, fmt.Errorf("e2b: read %s %s: unexpected status %s", sandboxID, path, resp.Status)
	}
	// max+1 so an over-size file is detectable without reading it all; the
	// clamps keep the arithmetic sane at the extremes.
	limit := max
	if limit < 0 {
		limit = 0
	}
	if limit < math.MaxInt64 {
		limit++
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("e2b: read %s %s: %w", sandboxID, path, err)
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("e2b: read %s %s: %w", sandboxID, path, substrate.ErrTooLarge)
	}
	return data, nil
}

// ReadFileRange reads at most limit bytes of path starting at offset,
// via envd's HTTP /files endpoint with a Range header. A missing path
// wraps substrate.ErrNotFound; an offset past the end yields no bytes.
// Used by the background-job output endpoints (2.6, #135) so a client
// can follow output without downloading the whole file.
func (c *Client) ReadFileRange(ctx context.Context, sandboxID, path string, offset, limit int64) ([]byte, error) {
	if offset < 0 {
		offset = 0
	}
	if limit < 0 {
		limit = 0
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, filesURL(c.cfg.ProxyURL, path), nil)
	if err != nil {
		return nil, fmt.Errorf("e2b: read range %s %s: %w", sandboxID, path, err)
	}
	if limit > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+limit-1))
	} else {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.envdClient(sandboxID, "").Do(req)
	if err != nil {
		return nil, fmt.Errorf("e2b: read range %s %s: %w", sandboxID, path, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		// The server ignored the Range header: skip the prefix itself.
		if offset > 0 {
			if _, err := io.CopyN(io.Discard, resp.Body, offset); err != nil && err != io.EOF {
				return nil, fmt.Errorf("e2b: read range %s %s: %w", sandboxID, path, err)
			}
		}
	case http.StatusPartialContent:
		// read below
	case http.StatusRequestedRangeNotSatisfiable:
		return nil, nil
	case http.StatusNotFound:
		return nil, fmt.Errorf("e2b: read range %s %s: %w", sandboxID, path, substrate.ErrNotFound)
	default:
		return nil, fmt.Errorf("e2b: read range %s %s: unexpected status %s", sandboxID, path, resp.Status)
	}
	// limit+1 so an over-size answer is detectable without reading it all.
	readLimit := limit
	if readLimit < math.MaxInt64 {
		readLimit++
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, readLimit))
	if err != nil {
		return nil, fmt.Errorf("e2b: read range %s %s: %w", sandboxID, path, err)
	}
	if int64(len(data)) > limit {
		data = data[:limit]
	}
	return data, nil
}

// Stat reports path's metadata through envd's filesystem.Stat.
func (c *Client) Stat(ctx context.Context, sandboxID, path string) (substrate.FileInfo, error) {
	resp, err := c.envdFilesystem(sandboxID, "").Stat(ctx, connect.NewRequest(&filesystem.StatRequest{Path: path}))
	if err != nil {
		return substrate.FileInfo{}, fmt.Errorf("e2b: stat %s %s: %w", sandboxID, path, mapConnectError(err))
	}
	return entryInfo(resp.Msg.GetEntry()), nil
}

// MakeDir creates path and any missing parents through envd's
// filesystem.MakeDir, then applies mode with a chmod exec (MakeDirRequest
// carries no mode).
func (c *Client) MakeDir(ctx context.Context, sandboxID, path string, mode os.FileMode) error {
	if mode == 0 {
		mode = 0o755
	}
	if _, err := c.envdFilesystem(sandboxID, "").MakeDir(ctx, connect.NewRequest(&filesystem.MakeDirRequest{Path: path})); err != nil {
		return fmt.Errorf("e2b: mkdir %s %s: %w", sandboxID, path, mapConnectError(err))
	}
	return c.chmod(ctx, sandboxID, path, mode)
}

// Remove deletes path. Recursive removes go through envd's filesystem.Remove,
// which deletes directories with all their contents. A non-recursive remove
// keeps POSIX semantics without recursion: files go through filesystem.Remove
// (it deletes a single file), directories through exec rmdir, which refuses a
// non-empty directory, so recursive=false never destroys contents.
func (c *Client) Remove(ctx context.Context, sandboxID, path string, recursive bool) error {
	fs := c.envdFilesystem(sandboxID, "")
	if recursive {
		if _, err := fs.Remove(ctx, connect.NewRequest(&filesystem.RemoveRequest{Path: path})); err != nil {
			return fmt.Errorf("e2b: remove %s %s: %w", sandboxID, path, mapConnectError(err))
		}
		return nil
	}
	info, err := c.Stat(ctx, sandboxID, path)
	if err != nil {
		return err
	}
	if !info.IsDir {
		if _, err := fs.Remove(ctx, connect.NewRequest(&filesystem.RemoveRequest{Path: path})); err != nil {
			return fmt.Errorf("e2b: remove %s %s: %w", sandboxID, path, mapConnectError(err))
		}
		return nil
	}
	r, err := c.Exec(ctx, sandboxID, substrate.ExecRequest{
		Args: []string{"/bin/rmdir", "--", path},
	})
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return rmdirError(sandboxID, path, r.ExitCode, r.Stderr)
	}
	return nil
}

// rmdirError maps a failed rmdir to the substrate's errors: "Directory
// not empty" (coreutils and busybox both say it) is ErrNotEmpty, which
// the files API answers with 409; anything else stays a plain failure.
func rmdirError(sandboxID, path string, exit int, stderr string) error {
	msg := strings.TrimSpace(stderr)
	if strings.Contains(strings.ToLower(msg), "not empty") {
		return fmt.Errorf("e2b: remove %s %s: %w", sandboxID, path, substrate.ErrNotEmpty)
	}
	return fmt.Errorf("e2b: remove %s %s: exit %d: %s", sandboxID, path, exit, msg)
}

// envdFilesystem returns the Connect client for one sandbox's envd filesystem
// service, with the same routing and auth headers the exec path uses.
func (c *Client) envdFilesystem(sandboxID, user string) filesystemconnect.FilesystemClient {
	h := envdHeaders{sandboxID: sandboxID, token: c.EnvdToken(sandboxID), user: defaultUser(user)}
	hc := &http.Client{Transport: &envdTransport{base: c.http.Transport, h: h}}
	return filesystemconnect.NewFilesystemClient(hc, c.cfg.ProxyURL, connect.WithInterceptors(&h))
}

// chmod applies mode to path with a guest chmod (envd's file APIs set no
// mode themselves).
func (c *Client) chmod(ctx context.Context, sandboxID, path string, mode os.FileMode) error {
	r, err := c.Exec(ctx, sandboxID, substrate.ExecRequest{
		Args: []string{"/bin/chmod", strconv.FormatUint(uint64(mode.Perm()), 8), "--", path},
	})
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("e2b: chmod %s %s: exit %d: %s", sandboxID, path, r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return nil
}

// precreate creates path empty with mode, plus any missing parent
// directories, before the upload: secrets written 0600 are never readable
// by others while the contents land. install refuses a directory path.
func (c *Client) precreate(ctx context.Context, sandboxID, path string, mode os.FileMode) error {
	r, err := c.Exec(ctx, sandboxID, substrate.ExecRequest{
		Args: []string{"/usr/bin/install", "-D", "-m", strconv.FormatUint(uint64(mode.Perm()), 8), "--", "/dev/null", path},
	})
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("e2b: create %s %s: exit %d: %s", sandboxID, path, r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return nil
}

// entryInfo converts an envd EntryInfo.
func entryInfo(e *filesystem.EntryInfo) substrate.FileInfo {
	mode := os.FileMode(e.GetMode() & 0o777)
	switch e.GetType() {
	case filesystem.FileType_FILE_TYPE_DIRECTORY:
		mode |= os.ModeDir
	case filesystem.FileType_FILE_TYPE_SYMLINK:
		mode |= os.ModeSymlink
	}
	return substrate.FileInfo{
		Name:    e.GetName(),
		Size:    e.GetSize(),
		Mode:    mode,
		ModTime: e.GetModifiedTime().AsTime(),
		IsDir:   e.GetType() == filesystem.FileType_FILE_TYPE_DIRECTORY,
	}
}

// uploadFile POSTs one multipart form part named "file" to envd's /files
// endpoint; envd creates the file's parent directories and replaces an
// existing file.
func uploadFile(ctx context.Context, hc *http.Client, baseURL, path string, data []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", baseName(path))
	if err != nil {
		return fmt.Errorf("e2b: upload %s: %w", path, err)
	}
	if _, err := part.Write(data); err != nil {
		return fmt.Errorf("e2b: upload %s: %w", path, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("e2b: upload %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, filesURL(baseURL, path), &buf)
	if err != nil {
		return fmt.Errorf("e2b: upload %s: %w", path, err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("e2b: upload %s: %w", path, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("e2b: upload %s: %w", path, substrate.ErrNotFound)
	default:
		return fmt.Errorf("e2b: upload %s: unexpected status %s", path, resp.Status)
	}
}

// filesURL builds envd's /files URL with the path query parameter.
func filesURL(baseURL, path string) string {
	return strings.TrimSuffix(baseURL, "/") + "/files?path=" + url.QueryEscape(path)
}

func baseName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// mapConnectError converts envd Connect errors to substrate sentinels.
func mapConnectError(err error) error {
	if connect.CodeOf(err) == connect.CodeNotFound {
		return fmt.Errorf("%w: %v", substrate.ErrNotFound, err)
	}
	return err
}
