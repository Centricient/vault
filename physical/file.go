package physical

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	log "github.com/mgutz/logxi/v1"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/vault/helper/jsonutil"
)

// FileBackend is a physical backend that stores data on disk
// at a given file path. It can be used for durable single server
// situations, or to develop locally where durability is not critical.
//
// WARNING: the file backend implementation is currently extremely unsafe
// and non-performant. It is meant mostly for local testing and development.
// It can be improved in the future.
type FileBackend struct {
	Path   string
	l      sync.Mutex
	logger log.Logger
}

// newFileBackend constructs a Filebackend using the given directory
func newFileBackend(conf map[string]string, logger log.Logger) (Backend, error) {
	path, ok := conf["path"]
	if !ok {
		return nil, fmt.Errorf("'path' must be set")
	}

	return &FileBackend{
		Path:   path,
		logger: logger,
	}, nil
}

func (b *FileBackend) Delete(path string) error {
	if path == "" {
		return nil
	}

	b.l.Lock()
	defer b.l.Unlock()

	_, fullPathPrefixedFileName, fullPathPrefixedEncodedFileName := b.path(path)
	err := os.Remove(fullPathPrefixedEncodedFileName)
	if err != nil && os.IsNotExist(err) {
		// For backwards compatibility, try to delete the file without base64
		// URL encoding the file name.
		err = os.Remove(fullPathPrefixedFileName)
	}

	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("Failed to remove %q: %v", path, err)
	}

	err = b.cleanupLogicalPath(path)

	return err
}

// cleanupLogicalPath is used to remove all empty nodes, begining with deepest
// one, aborting on first non-empty one, up to top-level node.
func (b *FileBackend) cleanupLogicalPath(path string) error {
	nodes := strings.Split(path, fmt.Sprintf("%c", os.PathSeparator))
	for i := len(nodes) - 1; i > 0; i-- {
		fullPath := filepath.Join(b.Path, filepath.Join(nodes[:i]...))

		dir, err := os.Open(fullPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			} else {
				return err
			}
		}

		list, err := dir.Readdir(1)
		dir.Close()
		if err != nil && err != io.EOF {
			return err
		}

		// If we have no entries, it's an empty directory; remove it
		if err == io.EOF || list == nil || len(list) == 0 {
			err = os.Remove(fullPath)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (b *FileBackend) Get(path string) (*Entry, error) {
	b.l.Lock()
	defer b.l.Unlock()

	_, fullPathPrefixedFileName, fullPathPrefixedEncodedFileName := b.path(path)
	f, err := os.Open(fullPathPrefixedEncodedFileName)
	if err != nil && os.IsNotExist(err) {
		// For backwards compatibility, if non-encoded file name is a valid
		// storage entry, read it out.
		f, err = os.Open(fullPathPrefixedFileName)
	}

	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var entry Entry
	if err := jsonutil.DecodeJSONFromReader(f, &entry); err != nil {
		return nil, err
	}

	return &entry, nil
}

func (b *FileBackend) Put(entry *Entry) error {
	var retErr error
	if entry == nil {
		retErr = multierror.Append(retErr, fmt.Errorf("nil entry"))
		return retErr
	}

	basePath, fullPathPrefixedFileName, fullPathPrefixedEncodedFileName := b.path(entry.Key)

	b.l.Lock()
	defer b.l.Unlock()

	// New storage entries will have their file names base64 URL encoded. If a
	// file with a non-encoded file name exists, it indicates that this is an
	// update operation. To avoid duplication of storage entries, delete the
	// old entry in the defer function.
	info, err := os.Stat(fullPathPrefixedFileName)
	if err == nil && info != nil {
		defer func() {
			err := os.Remove(fullPathPrefixedFileName)
			if err != nil && !os.IsNotExist(err) {
				retErr = multierror.Append(retErr, fmt.Errorf("failed to remove old entry: %v", err))
				return
			}
			err = b.cleanupLogicalPath(entry.Key)
			if err != nil {
				retErr = multierror.Append(retErr, fmt.Errorf("failed to cleanup the after removing old entry: %v", err))
				return
			}
		}()
	}

	// Make the parent tree
	if err := os.MkdirAll(basePath, 0755); err != nil {
		retErr = multierror.Append(retErr, err)
		return retErr
	}

	// JSON encode the entry and write it
	f, err := os.OpenFile(
		fullPathPrefixedEncodedFileName,
		os.O_CREATE|os.O_TRUNC|os.O_WRONLY,
		0600)
	if err != nil {
		retErr = multierror.Append(retErr, err)
		return retErr
	}
	defer f.Close()
	enc := json.NewEncoder(f)

	err = enc.Encode(entry)
	if err != nil {
		retErr = multierror.Append(retErr, err)
		return retErr
	}
	return nil
}

func (b *FileBackend) List(prefix string) ([]string, error) {
	b.l.Lock()
	defer b.l.Unlock()

	path := b.Path
	if prefix != "" {
		path = filepath.Join(path, prefix)
	}

	// Read the directory contents
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, err
	}
	defer f.Close()

	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}

	for i, name := range names {
		if name[0] == '_' {
			names[i] = name[1:]
			// If the file name is encoded, decode it to retain the list output
			// meaningful.
			nameDecodedBytes, err := base64.URLEncoding.DecodeString(names[i])
			if err == nil {
				names[i] = string(nameDecodedBytes)
			}
		} else {
			names[i] = name + "/"
		}
	}

	return names, nil
}

func (b *FileBackend) path(path string) (string, string, string) {
	fullPath := filepath.Join(b.Path, path)

	basePath := filepath.Dir(fullPath)

	fileName := filepath.Base(fullPath)

	fullPathPrefixedFileName := filepath.Join(basePath, "_"+fileName)

	// base64 URL encode the file name to make all the characters compatible by
	// the host OS (specially Windows). However, the basePath can contain
	// disallowed characters.  Encoding all the directory names and the file
	// name is an over kill, and encoding the fullPath will flatten the
	// storage, which *may* not be desired.
	fullPathPrefixedEncodedFileName := filepath.Join(basePath, "_"+base64.URLEncoding.EncodeToString([]byte(fileName)))

	return basePath, fullPathPrefixedFileName, fullPathPrefixedEncodedFileName
}
