package rardecode

import (
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"time"
)

// FileHeader HostOS types
const (
	HostOSUnknown = 0
	HostOSMSDOS   = 1
	HostOSOS2     = 2
	HostOSWindows = 3
	HostOSUnix    = 4
	HostOSMacOS   = 5
	HostOSBeOS    = 6
)

const (
	maxPassword = 128
)

var (
	errShortFile        = errors.New("rardecode: decoded file too short")
	errInvalidFileBlock = errors.New("rardecode: invalid file block")
	errUnexpectedArcEnd = errors.New("rardecode: unexpected end of archive")
	errBadFileChecksum  = errors.New("rardecode: bad file checksum")
)

type limitedReader struct {
	r        io.Reader
	n        int64 // bytes remaining
	shortErr error // error returned when r returns io.EOF with n > 0
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > l.n {
		p = p[0:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	if err == io.EOF && l.n > 0 {
		return n, l.shortErr
	}
	return n, err
}

// limitReader returns an io.Reader that reads from r and stops with
// io.EOF after n bytes.
// If r returns an io.EOF before reading n bytes, err is returned.
func limitReader(r io.Reader, n int64, err error) io.Reader {
	return &limitedReader{r, n, err}
}

// fileChecksum allows file checksum validations to be performed.
// File contents must first be written to fileChecksum. Then valid is
// called to perform the file checksum calculation to determine
// if the file contents are valid or not.
type fileChecksum interface {
	io.Writer
	valid() bool
}

// FileHeader represents a single file in a RAR archive.
type FileHeader struct {
	Name             string    // file name using '/' as the directory separator
	IsDir            bool      // is a directory
	HostOS           byte      // Host OS the archive was created on
	Attributes       int64     // file attributes
	PackedSize       int64     // packed file size (or first block if the file spans volumes)
	UnPackedSize     int64     // unpacked file size
	UnKnownSize      bool      // unpacked file size is not known
	ModificationTime time.Time // modification time (non-zero if set)
	CreationTime     time.Time // creation time (non-zero if set)
	AccessTime       time.Time // access time (non-zero if set)
	Version          int       // file version
}

// fileBlockHeader represents a file block in a RAR archive.
// Files may comprise one or more file blocks.
// Solid files retain decode tables and dictionary from previous solid files in the archive.
type fileBlockHeader struct {
	first   bool         // first block in file
	last    bool         // last block in file
	solid   bool         // file is solid
	winSize uint         // log base 2 of decode window size
	cksum   fileChecksum // file checksum
	decoder decoder      // decoder to use for file
	key     []byte       // key for AES, non-empty if file encrypted
	iv      []byte       // iv for AES, non-empty if file encrypted
	FileHeader
}

// fileBlockReader provides sequential access to file blocks in a RAR archive.
type fileBlockReader interface {
	io.Reader                        // Read's read data from the current file block
	next() (*fileBlockHeader, error) // advances to the next file block
	reset(r io.Reader)               // resets for new volume file
	isSolid() bool                   // is archive solid
	version() int                    // returns current archive format version
}

// packedFileReader provides sequential access to packed files in a RAR archive.
type packedFileReader struct {
	r fileBlockReader
	h *fileBlockHeader // current file header
}

// nextBlockInFile advances to the next file block in the current file, or returns
// an error if there is a problem.
// It is invalid to call this when already at the last block in the current file.
func (f *packedFileReader) nextBlockInFile() error {
	h, err := f.r.next()
	if err != nil {
		if err == io.EOF {
			// archive ended, but file hasn't
			return errUnexpectedArcEnd
		}
		return err
	}
	if h.first || h.Name != f.h.Name {
		return errInvalidFileBlock
	}
	f.h = h
	return nil
}

// next advances to the next packed file in the RAR archive.
func (f *packedFileReader) next() (*fileBlockHeader, error) {
	if f.h != nil {
		// skip to last block in current file
		for !f.h.last {
			if err := f.nextBlockInFile(); err != nil {
				return nil, err
			}
		}
	}
	var err error
	f.h, err = f.r.next() // get next file block
	if err != nil {
		return nil, err
	}
	if !f.h.first {
		return nil, errInvalidFileBlock
	}
	return f.h, nil
}

// Read reads the packed data for the current file into p.
func (f *packedFileReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p) // read current block data
	for err == io.EOF {   // current block empty
		if n > 0 {
			return n, nil
		}
		if f.h == nil || f.h.last {
			return 0, io.EOF // last block so end of file
		}
		if err := f.nextBlockInFile(); err != nil {
			return 0, err
		}
		n, err = f.r.Read(p) // read new block data
	}
	return n, err
}

// Reader provides sequential access to files in a RAR archive.
type Reader struct {
	r      io.Reader        // reader for current unpacked file
	pr     packedFileReader // reader for current packed file
	dr     decodeReader     // reader for decoding and filters if file is compressed
	cksum  fileChecksum     // current file checksum
	solidr io.Reader        // reader for solid file
}

// Read reads from the current file in the RAR archive.
func (r *Reader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err == io.EOF && r.cksum != nil && !r.cksum.valid() {
		return n, errBadFileChecksum
	}
	return n, err
}

// Next advances to the next file in the archive.
func (r *Reader) Next() (*FileHeader, error) {
	if r.solidr != nil {
		// solid files must be read fully to update decoder information
		if _, err := io.Copy(ioutil.Discard, r.solidr); err != nil {
			return nil, err
		}
	}

	h, err := r.pr.next() // skip to next file
	if err != nil {
		return nil, err
	}
	r.solidr = nil

	r.r = io.Reader(&r.pr) // start with packed file reader

	// check for encryption
	if len(h.key) > 0 && len(h.iv) > 0 {
		r.r = newAesDecryptReader(r.r, h.key, h.iv) // decrypt
	}
	// check for compression
	if h.decoder != nil {
		err = r.dr.init(r.r, h.decoder, h.winSize, !h.solid)
		if err != nil {
			return nil, err
		}
		r.r = &r.dr
		if r.pr.r.isSolid() {
			r.solidr = r.r
		}
	}
	if h.UnPackedSize >= 0 && !h.UnKnownSize {
		// Limit reading to UnPackedSize as there may be padding
		r.r = limitReader(r.r, h.UnPackedSize, errShortFile)
	}
	r.cksum = h.cksum
	if r.cksum != nil {
		r.r = io.TeeReader(r.r, h.cksum) // write file data to checksum as it is read
	}
	fh := new(FileHeader)
	*fh = h.FileHeader
	return fh, nil
}

func (r *Reader) init(fbr fileBlockReader) {
	r.r = bytes.NewReader(nil) // initial reads will always return EOF
	r.pr.r = fbr
}

// NewReader creates a Reader reading from r.
func NewReader(r io.Reader, password string) (*Reader, error) {
	fbr, err := newFileBlockReader(r, password)
	if err != nil {
		return nil, err
	}
	rr := new(Reader)
	rr.init(fbr)
	return rr, nil
}

type ReadCloser struct {
	v *volume
	Reader
}

// Close closes the rar file.
func (rc *ReadCloser) Close() error {
	return rc.v.Close()
}

// OpenReader opens a RAR archive specified by the name and returns a ReadCloser.
func OpenReader(name, password string) (*ReadCloser, error) {
	v, err := openVolume(name, password)
	if err != nil {
		return nil, err
	}
	rc := new(ReadCloser)
	rc.v = v
	rc.Reader.init(v)
	return rc, nil
}