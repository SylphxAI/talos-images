// Package bootinventory provides bounded, metadata-only inventory of an EFI filesystem.
// It has no device opener, mount, writer, network client or lifecycle operation.
package bootinventory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxFiles bounds both response size and traversal memory, including directories.
	MaxFiles                 = 4096
	MaxDepth                 = 8
	MaxReadBytes       int64 = 3 << 30
	ReadBytesPerSecond int64 = 32 << 20
	MaxOutputBytes           = 2 << 20
	EFIPartitionType         = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"
)

// Volume is a projection of discovered block metadata, never caller-selected paths.
type Volume struct {
	ID, Path, ParentID, ParentPath, Type, Label, PartitionType, UUID, Filesystem string
	Index                                                                        uint
	Bytes                                                                        uint64
}

// SelectEFI refuses missing, ambiguous or inconsistent system-disk EFI identity.
func SelectEFI(diskID, diskPath string, volumes []Volume) (Volume, error) {
	if !deviceName(diskID) || diskPath != "/dev/"+diskID {
		return Volume{}, errors.New("invalid authoritative system disk identity")
	}
	var candidates []Volume
	for _, v := range volumes {
		if v.ParentID == diskID && v.Label == "EFI" {
			candidates = append(candidates, v)
		}
	}
	if len(candidates) != 1 {
		return Volume{}, errors.New("expected exactly one system-disk EFI partition")
	}
	v := candidates[0]
	if !deviceName(v.ID) || v.Path != "/dev/"+v.ID || v.ParentPath != diskPath || v.Type != "partition" ||
		!strings.EqualFold(v.PartitionType, EFIPartitionType) || !uuid(v.UUID) || v.Index == 0 ||
		v.Filesystem != "vfat" || v.Bytes < 512 || v.Bytes > 8<<30 {
		return Volume{}, errors.New("inconsistent system-disk EFI partition metadata")
	}
	return v, nil
}

func deviceName(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func uuid(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// File contains no file contents, command line or embedded UKI/configuration values.
type File struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Result is evidence of observed files, not a recovery or fallback verdict.
type Result struct {
	SchemaVersion  int    `json:"schema_version"`
	SystemDisk     string `json:"system_disk"`
	Partition      string `json:"partition"`
	PartitionUUID  string `json:"partition_uuid"`
	PartitionBytes uint64 `json:"partition_bytes"`
	BootID         string `json:"boot_id"`
	BootedEntry    string `json:"booted_entry"`
	Files          []File `json:"files"`
	ReadBytes      int64  `json:"read_bytes"`
	SnapshotAtomic bool   `json:"snapshot_atomic"`
}

// ValidateBootMetadata permits only the boot ID and an EFI filename, never contents.
func ValidateBootMetadata(bootID, entry string) error {
	if !uuid(bootID) || !safeName(entry) || !strings.HasSuffix(strings.ToLower(entry), ".efi") {
		return errors.New("invalid boot metadata")
	}
	return nil
}

func safeName(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 255 || !utf8.ValidString(s) || strings.ContainsAny(s, "/\\") {
		return false
	}
	for _, c := range s {
		if !unicode.IsPrint(c) || unicode.Is(unicode.Cf, c) {
			return false
		}
	}
	return true
}

// Scan reads only through fs.FS. The shell must supply a confined, read-only EFI
// filesystem; it cannot supply a writable device or arbitrary host filesystem.
// No partial result is returned on cancellation, malformed entries or bounds.
func Scan(ctx context.Context, filesystem fs.FS) ([]File, int64, error) {
	return scan(ctx, filesystem, func(ctx context.Context, elapsed time.Duration, bytes int64) error {
		delay := time.Duration(bytes)*time.Second/time.Duration(ReadBytesPerSecond) - elapsed
		if delay <= 0 {
			return ctx.Err()
		}
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	})
}

// scan keeps timing injectable for deterministic boundary tests.
func scan(ctx context.Context, filesystem fs.FS, pace func(context.Context, time.Duration, int64) error) ([]File, int64, error) {
	started := time.Now()
	files := []File{}
	var readBytes int64
	entries, outputBytes := 0, 0
	var walk func(string, int) error
	walk = func(dir string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > MaxDepth {
			return errors.New("EFI directory depth exceeded")
		}
		f, err := filesystem.Open(dir)
		if err != nil {
			return errors.New("EFI directory open failed")
		}
		defer f.Close()
		d, ok := f.(fs.ReadDirFile)
		if !ok {
			return errors.New("EFI directory cannot be enumerated")
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			batch, err := d.ReadDir(32)
			if err != nil && err != io.EOF {
				return errors.New("EFI directory read failed")
			}
			if len(batch) == 0 && err == nil {
				return errors.New("EFI directory made no progress")
			}
			for _, entry := range batch {
				entries++
				if entries > MaxFiles || !safeName(entry.Name()) {
					return errors.New("EFI entry population/name bound violated")
				}
				name := path.Join(dir, entry.Name())
				if entry.Type()&fs.ModeSymlink != 0 {
					return errors.New("EFI symlinks are not supported")
				}
				if entry.IsDir() {
					if err := walk(name, depth+1); err != nil {
						return err
					}
					continue
				}
				info, err := entry.Info()
				if err != nil {
					return errors.New("EFI entry stat failed")
				}
				if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > MaxReadBytes-readBytes {
					return errors.New("EFI file type or byte bound violation")
				}
				digest, _, err := hashFile(ctx, filesystem, name, info, func(n int64) error { readBytes += n; return pace(ctx, time.Since(started), readBytes) })
				if err != nil {
					return err
				}
				outputBytes += len(name) + 128
				if outputBytes > MaxOutputBytes {
					return errors.New("EFI output byte bound exceeded")
				}
				files = append(files, File{Path: "/" + name, Bytes: info.Size(), SHA256: digest})
			}
			if err == io.EOF {
				return nil
			}
		}
	}
	if err := walk(".", 0); err != nil {
		return nil, readBytes, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, readBytes, nil
}

func hashFile(ctx context.Context, filesystem fs.FS, name string, info fs.FileInfo, pace func(int64) error) (digest string, readBytes int64, err error) {
	f, err := filesystem.Open(name)
	if err != nil {
		return "", 0, errors.New("EFI file open failed")
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() != info.Size() {
		return "", 0, errors.New("EFI file changed before reading")
	}
	h := sha256.New()
	buf := make([]byte, 64<<10)
	remaining := info.Size()
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return "", readBytes, err
		}
		chunk := min(int64(len(buf)), remaining)
		n, err := io.ReadFull(f, buf[:chunk])
		readBytes += int64(n)
		remaining -= int64(n)
		if err != nil {
			return "", readBytes, errors.New("short EFI file read")
		}
		h.Write(buf[:n])
		if err := pace(int64(n)); err != nil {
			return "", readBytes, err
		}
	}
	var extra [1]byte
	n, readErr := f.Read(extra[:])
	after, statErr := f.Stat()
	if n != 0 || readErr != io.EOF || statErr != nil || after.Size() != info.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return "", readBytes, errors.New("EFI file changed while reading")
	}
	return hex.EncodeToString(h.Sum(nil)), readBytes, nil
}

// CheckResult checks the metadata contract before a client writes any output.
func CheckResult(r Result) error {
	if r.SchemaVersion != 1 || !deviceName(strings.TrimPrefix(r.SystemDisk, "/dev/")) || r.SystemDisk != "/dev/"+path.Base(r.SystemDisk) || !deviceName(strings.TrimPrefix(r.Partition, "/dev/")) || r.Partition != "/dev/"+path.Base(r.Partition) || r.SystemDisk == r.Partition || !uuid(r.PartitionUUID) || r.PartitionBytes < 512 || r.PartitionBytes > 8<<30 || r.SnapshotAtomic || r.ReadBytes < 0 || r.ReadBytes > MaxReadBytes || len(r.Files) > MaxFiles {
		return errors.New("invalid EFI inventory response")
	}
	if err := ValidateBootMetadata(r.BootID, r.BootedEntry); err != nil {
		return err
	}
	seen := map[string]bool{}
	var total int64
	for _, f := range r.Files {
		if !strings.HasPrefix(f.Path, "/") || !fs.ValidPath(strings.TrimPrefix(f.Path, "/")) || f.Bytes < 0 || f.Bytes > MaxReadBytes-total || len(f.SHA256) != 64 || seen[f.Path] {
			return errors.New("invalid EFI file metadata")
		}
		if strings.Count(f.Path, "/") > MaxDepth+1 {
			return errors.New("EFI file path depth exceeded")
		}
		for _, part := range strings.Split(strings.TrimPrefix(f.Path, "/"), "/") {
			if !safeName(part) {
				return fmt.Errorf("invalid EFI filename")
			}
		}
		if _, err := hex.DecodeString(f.SHA256); err != nil {
			return errors.New("invalid EFI file digest")
		}
		seen[f.Path] = true
		total += f.Bytes
	}
	if total != r.ReadBytes || uint64(total) > r.PartitionBytes {
		return errors.New("inconsistent EFI read byte accounting")
	}
	return nil
}

// Encode returns a complete bounded metadata document; no partial output on error.
func Encode(r Result) ([]byte, error) {
	if err := CheckResult(r); err != nil {
		return nil, err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if len(data) >= MaxOutputBytes {
		return nil, errors.New("EFI output byte bound exceeded")
	}
	return append(data, '\n'), nil
}

// Decode refuses unknown fields, excess bytes and trailing documents before output.
func Decode(reader io.Reader) (Result, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxOutputBytes+1))
	if err != nil {
		return Result{}, err
	}
	if len(data) > MaxOutputBytes {
		return Result{}, errors.New("EFI response byte bound exceeded")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result Result
	if err = decoder.Decode(&result); err != nil {
		return Result{}, err
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF {
		return Result{}, errors.New("invalid trailing EFI response")
	}
	if err = CheckResult(result); err != nil {
		return Result{}, err
	}
	return result, nil
}
