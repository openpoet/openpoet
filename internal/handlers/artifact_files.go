package handlers

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"openpoet/internal/application"
	"openpoet/internal/database"
	"openpoet/internal/files"
)

// An artifact can be published from a file (html_path, or markdown_path for a
// markdown version) and its content written to a file (output_path), so a
// session does not have to pass a large document through a tool argument in
// either direction.
//
// The file is read or written by OpenPoet on the machine the session runs on
// (the local disk, or over SFTP for a remote project), and only for a session:
// the path is resolved against that session's workspace and has to stay inside
// the workspace or the machine's temporary directory, after symbolic links are
// followed. OpenPoet reaches that machine with the same OS account the session
// runs as, so this gives a session no file it could not already read or write
// itself; the limits (a regular .html/.htm file, or .md/.markdown for a
// markdown version, inside those directories, at most the artifact size limit)
// keep the tool from being pointed at anything that is not a document being
// published.

// artifactFileError is a refusal the caller can act on.
type artifactFileError struct {
	code    string
	message string
}

func (e *artifactFileError) Error() string { return e.message }

func artifactFileErrorf(code, format string, args ...any) error {
	return &artifactFileError{code: code, message: fmt.Sprintf(format, args...)}
}

func respondArtifactFileError(w http.ResponseWriter, err error) {
	var fileErr *artifactFileError
	if errors.As(err, &fileErr) {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": fileErr.message, "code": fileErr.code})
		return
	}
	respondError(w, http.StatusInternalServerError, err.Error())
}

// workspaceFS is the part of a file system the artifact file access needs. The
// local disk and an SFTP connection both provide it.
type workspaceFS interface {
	// RealPath returns the absolute path with symbolic links resolved. It
	// fails when the path does not exist.
	RealPath(name string) (string, error)
	Lstat(name string) (os.FileInfo, error)
	Open(name string) (io.ReadCloser, error)
	Create(name string) (io.WriteCloser, error)
	IsAbs(name string) bool
	Join(elem ...string) string
	Dir(name string) string
	Base(name string) string
	Separator() string
}

type localWorkspaceFS struct{}

func (localWorkspaceFS) RealPath(name string) (string, error) {
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}
func (localWorkspaceFS) Lstat(name string) (os.FileInfo, error)  { return os.Lstat(name) }
func (localWorkspaceFS) Open(name string) (io.ReadCloser, error) { return os.Open(name) }
func (localWorkspaceFS) Create(name string) (io.WriteCloser, error) {
	return os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
}
func (localWorkspaceFS) IsAbs(name string) bool     { return filepath.IsAbs(name) }
func (localWorkspaceFS) Join(elem ...string) string { return filepath.Join(elem...) }
func (localWorkspaceFS) Dir(name string) string     { return filepath.Dir(name) }
func (localWorkspaceFS) Base(name string) string    { return filepath.Base(name) }
func (localWorkspaceFS) Separator() string          { return string(filepath.Separator) }

// sftpWorkspaceFS is the workspace of a remote (POSIX) project.
type sftpWorkspaceFS struct{ client *sftp.Client }

func (f sftpWorkspaceFS) RealPath(name string) (string, error) {
	resolved, err := f.client.RealPath(name)
	if err != nil {
		return "", err
	}
	// Some servers canonicalize a path that does not exist; make it an error
	// here, as it is on the local disk.
	if _, err := f.client.Lstat(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}
func (f sftpWorkspaceFS) Lstat(name string) (os.FileInfo, error)     { return f.client.Lstat(name) }
func (f sftpWorkspaceFS) Open(name string) (io.ReadCloser, error)    { return f.client.Open(name) }
func (f sftpWorkspaceFS) Create(name string) (io.WriteCloser, error) { return f.client.Create(name) }
func (sftpWorkspaceFS) IsAbs(name string) bool                       { return path.IsAbs(name) }
func (sftpWorkspaceFS) Join(elem ...string) string                   { return path.Join(elem...) }
func (sftpWorkspaceFS) Dir(name string) string                       { return path.Dir(name) }
func (sftpWorkspaceFS) Base(name string) string                      { return path.Base(name) }
func (sftpWorkspaceFS) Separator() string                            { return "/" }

// artifactWorkspace is where a session's artifact files may live: roots[0] is
// the session's working directory (relative paths resolve against it), the
// rest are the temporary directories of that machine.
type artifactWorkspace struct {
	fs    workspaceFS
	roots []string
	close func()
}

// remoteTempRoots are the temporary directories of a remote POSIX machine
// (Linux and macOS, where /tmp is a link into /private and the per-user
// temporary directory is under /var/folders).
var remoteTempRoots = []string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}

func localTempRoots() []string {
	roots := []string{os.TempDir()}
	if os.PathSeparator == '/' && roots[0] != "/tmp" {
		roots = append(roots, "/tmp")
	}
	return roots
}

// artifactWorkspaceFor opens the workspace of the session a request is
// attributed to. The caller must call close on the result.
func (a *API) artifactWorkspaceFor(r *http.Request, claimedSessionID string) (*artifactWorkspace, error) {
	sessionID := artifactSessionID(r, claimedSessionID)
	if sessionID == "" {
		return nil, artifactFileErrorf("session_required", "A file path is only available to a session: it is resolved inside that session's workspace. Pass the document in html instead")
	}
	session, err := a.db.GetSession(r.Context(), sessionID)
	if err != nil || session == nil || session.ID == "" {
		return nil, artifactFileErrorf("session_required", "Session %s was not found, so its workspace is unknown. Pass the document in html instead", sessionID)
	}
	project, err := a.db.GetProject(r.Context(), session.ProjectID)
	if err != nil || project == nil {
		return nil, artifactFileErrorf("workspace_unavailable", "The project of session %s was not found, so its workspace is unknown", sessionID)
	}
	root := strings.TrimSpace(session.WorkDir)
	if root == "" {
		root = strings.TrimSpace(project.Path)
	}
	if project.Type == "local" {
		if !filepath.IsAbs(root) {
			return nil, artifactFileErrorf("workspace_unavailable", "The workspace of this session is not an absolute path")
		}
		return &artifactWorkspace{fs: localWorkspaceFS{}, roots: append([]string{root}, localTempRoots()...), close: func() {}}, nil
	}
	if project.TargetsWindows() {
		return nil, artifactFileErrorf("workspace_unavailable", "File paths are not available for a remote Windows project. Pass the document in html, and use patch_artifact for changes")
	}
	if !path.IsAbs(root) {
		return nil, artifactFileErrorf("workspace_unavailable", "The workspace of this session is not an absolute path")
	}
	var sshClient *ssh.Client
	var sftpClient *sftp.Client
	if sshClient, sftpClient, err = files.NewRemoteFileManager(project, a.DecryptFunc()).NewSFTPConnector().Connect(); err != nil {
		return nil, artifactFileErrorf("workspace_unavailable", "Could not reach the remote workspace of this session: %v", err)
	}
	return &artifactWorkspace{
		fs: sftpWorkspaceFS{client: sftpClient}, roots: append([]string{root}, remoteTempRoots...),
		close: func() { _ = sftpClient.Close(); _ = sshClient.Close() },
	}, nil
}

// artifactFileExtensions are the file names a version of each format may be
// read from or written to.
var artifactFileExtensions = map[string][]string{
	database.ArtifactFormatHTML:     {".html", ".htm"},
	database.ArtifactFormatMarkdown: {".md", ".markdown"},
}

func isArtifactFileName(name, format string) bool {
	lower := strings.ToLower(name)
	for _, extension := range artifactFileExtensions[format] {
		if strings.HasSuffix(lower, extension) {
			return true
		}
	}
	return false
}

// artifactFileKind names the files a format accepts, for an error message.
func artifactFileKind(format string) string {
	if format == database.ArtifactFormatMarkdown {
		return "a .md or .markdown file"
	}
	return "an .html or .htm file"
}

// within reports whether resolved is root or a path under it.
func (w *artifactWorkspace) within(root, resolved string) bool {
	separator := w.fs.Separator()
	root = strings.TrimRight(root, separator)
	if root == "" {
		return false // never the file system root
	}
	return resolved == root || strings.HasPrefix(resolved, root+separator)
}

// allowed reports whether a resolved path is inside the workspace or a
// temporary directory, comparing against those directories resolved the same
// way.
func (w *artifactWorkspace) allowed(resolved string) bool {
	for _, root := range w.roots {
		rootResolved, err := w.fs.RealPath(root)
		if err != nil {
			continue
		}
		if w.within(rootResolved, resolved) {
			return true
		}
	}
	return false
}

func (w *artifactWorkspace) absolute(name, format string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsRune(name, 0) {
		return "", artifactFileErrorf("invalid_path", "The file path is empty or invalid")
	}
	if !isArtifactFileName(name, format) {
		return "", artifactFileErrorf("invalid_path", "The file must be %s: %s", artifactFileKind(format), name)
	}
	if !w.fs.IsAbs(name) {
		name = w.fs.Join(w.roots[0], name)
	}
	return name, nil
}

const artifactFileOutsideMessage = "%s is outside this session's workspace (%s) and the temporary directory. Put the file inside one of them"

// read returns the content of an HTML file of the workspace and the resolved
// path it was read from.
func (w *artifactWorkspace) read(name string, maxBytes int64) (string, string, error) {
	return w.readFormat(name, database.ArtifactFormatHTML, maxBytes)
}

// readFormat is read for a file of the given format (an HTML page or a
// markdown file).
func (w *artifactWorkspace) readFormat(name, format string, maxBytes int64) (string, string, error) {
	name, err := w.absolute(name, format)
	if err != nil {
		return "", "", err
	}
	resolved, err := w.fs.RealPath(name)
	if err != nil {
		return "", "", artifactFileErrorf("file_not_found", "File not found: %s (relative paths are resolved against the session's workspace, %s)", name, w.roots[0])
	}
	if !w.allowed(resolved) {
		return "", "", artifactFileErrorf("path_not_allowed", artifactFileOutsideMessage, resolved, w.roots[0])
	}
	if !isArtifactFileName(resolved, format) {
		return "", "", artifactFileErrorf("invalid_path", "The file must be %s: %s", artifactFileKind(format), resolved)
	}
	info, err := w.fs.Lstat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", artifactFileErrorf("invalid_path", "%s is not a regular file", resolved)
	}
	if info.Size() > maxBytes {
		return "", "", artifactFileErrorf("content_too_large", "%s is %d bytes; the limit is %d bytes per version", resolved, info.Size(), maxBytes)
	}
	file, err := w.fs.Open(resolved)
	if err != nil {
		return "", "", artifactFileErrorf("file_not_found", "Could not open %s: %v", resolved, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return "", "", artifactFileErrorf("file_not_found", "Could not read %s: %v", resolved, err)
	}
	if int64(len(data)) > maxBytes {
		return "", "", artifactFileErrorf("content_too_large", "%s is larger than the limit of %d bytes per version", resolved, maxBytes)
	}
	return string(data), resolved, nil
}

// write stores content in an HTML file of the workspace, replacing an existing
// regular file, and returns the resolved path. The directory must exist.
func (w *artifactWorkspace) write(name, content string) (string, error) {
	return w.writeFormat(name, database.ArtifactFormatHTML, content)
}

// writeFormat is write for a version of the given format: the file name has to
// match what is written (a markdown version goes to a .md file).
func (w *artifactWorkspace) writeFormat(name, format, content string) (string, error) {
	name, err := w.absolute(name, format)
	if err != nil {
		return "", err
	}
	directory, err := w.fs.RealPath(w.fs.Dir(name))
	if err != nil {
		return "", artifactFileErrorf("file_not_found", "The directory of %s does not exist (relative paths are resolved against the session's workspace, %s)", name, w.roots[0])
	}
	if !w.allowed(directory) {
		return "", artifactFileErrorf("path_not_allowed", artifactFileOutsideMessage, directory, w.roots[0])
	}
	if info, err := w.fs.Lstat(directory); err != nil || !info.IsDir() {
		return "", artifactFileErrorf("invalid_path", "%s is not a directory", directory)
	}
	target := w.fs.Join(directory, w.fs.Base(name))
	// Only a regular file is replaced: a link there would send the write
	// somewhere else.
	if info, err := w.fs.Lstat(target); err == nil && !info.Mode().IsRegular() {
		return "", artifactFileErrorf("invalid_path", "%s exists and is not a regular file", target)
	}
	file, err := w.fs.Create(target)
	if err != nil {
		return "", artifactFileErrorf("write_failed", "Could not write %s: %v", target, err)
	}
	if _, err := io.WriteString(file, content); err != nil {
		_ = file.Close()
		return "", artifactFileErrorf("write_failed", "Could not write %s: %v", target, err)
	}
	if err := file.Close(); err != nil {
		return "", artifactFileErrorf("write_failed", "Could not write %s: %v", target, err)
	}
	return target, nil
}

// artifactContentRequest is the content of a create or update request: an HTML
// document (html, or the file html_path names) or markdown source (markdown,
// or the file markdown_path names). The paths are files of the session's
// workspace.
type artifactContentRequest struct {
	HTML         string `json:"html"`
	HTMLPath     string `json:"html_path"`
	Markdown     string `json:"markdown"`
	MarkdownPath string `json:"markdown_path"`
}

// artifactContentInput returns the content a create or update request carries
// and its format. Exactly one of the four fields names it (none is left to the
// service, which answers that the content is required). source is the resolved
// path when the content came from a file.
func (a *API) artifactContentInput(r *http.Request, input artifactContentRequest, claimedSessionID string) (content, format, source string, err error) {
	htmlPath, markdownPath := strings.TrimSpace(input.HTMLPath), strings.TrimSpace(input.MarkdownPath)
	given := 0
	for _, present := range []bool{strings.TrimSpace(input.HTML) != "", htmlPath != "", strings.TrimSpace(input.Markdown) != "", markdownPath != ""} {
		if present {
			given++
		}
	}
	if given > 1 {
		return "", "", "", artifactFileErrorf("invalid_request", "Pass only one of html, html_path, markdown and markdown_path")
	}
	format, filePath := database.ArtifactFormatHTML, htmlPath
	if strings.TrimSpace(input.Markdown) != "" || markdownPath != "" {
		format, filePath = database.ArtifactFormatMarkdown, markdownPath
	}
	if filePath == "" {
		if format == database.ArtifactFormatMarkdown {
			return input.Markdown, format, "", nil
		}
		return input.HTML, format, "", nil
	}
	workspace, err := a.artifactWorkspaceFor(r, claimedSessionID)
	if err != nil {
		return "", "", "", err
	}
	defer workspace.close()
	content, source, err = workspace.readFormat(filePath, format, application.MaxArtifactHTMLBytes)
	return content, format, source, err
}
