package cutovercanary

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type logSnapshot struct {
	path   string
	info   os.FileInfo
	offset int64
}

func snapshotCandidateLogs(paths []string) ([]logSnapshot, error) {
	if len(paths) < 1 || len(paths) > 16 {
		return nil, errors.New("candidate log list must contain 1..16 files")
	}
	seen := map[string]bool{}
	out := make([]logSnapshot, 0, len(paths))
	for _, path := range paths {
		if !filepath.IsAbs(path) || seen[filepath.Clean(path)] {
			return nil, errors.New("candidate log paths must be distinct absolute files")
		}
		seen[filepath.Clean(path)] = true
		info, err := os.Lstat(path)
		if err != nil {
			return nil, errors.New("candidate log unavailable")
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("candidate log must be a regular non-symlink")
		}
		out = append(out, logSnapshot{path: path, info: info, offset: info.Size()})
	}
	return out, nil
}

func freshProxyLogEvidence(snapshots []logSnapshot, agent, sessionID, markerHash string, notBefore time.Time, maxAppend int64) (bool, error) {
	if maxAppend < 256 || maxAppend > 1<<20 {
		return false, errors.New("candidate log append cap must be 256 bytes..1 MiB")
	}
	found := false
	var totalAppend int64
	for _, snapshot := range snapshots {
		pathInfo, err := os.Lstat(snapshot.path)
		if err != nil || !pathInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(snapshot.info, pathInfo) {
			return false, errors.New("candidate log rotated or replaced")
		}
		f, err := os.Open(snapshot.path)
		if err != nil {
			return false, errors.New("candidate log unavailable after witness")
		}
		info, statErr := f.Stat()
		if statErr != nil {
			f.Close()
			return false, errors.New("candidate log stat failed")
		}
		if !os.SameFile(snapshot.info, info) || info.Size() < snapshot.offset {
			f.Close()
			return false, errors.New("candidate log rotated or truncated")
		}
		appendSize := info.Size() - snapshot.offset
		totalAppend += appendSize
		if appendSize > maxAppend || totalAppend > maxAppend {
			f.Close()
			return false, errors.New("candidate log append exceeded cap")
		}
		if _, err := f.Seek(snapshot.offset, io.SeekStart); err != nil {
			f.Close()
			return false, errors.New("candidate log seek failed")
		}
		b, err := io.ReadAll(io.LimitReader(f, maxAppend+1))
		f.Close()
		if err != nil || int64(len(b)) > maxAppend {
			return false, errors.New("candidate log append read failed")
		}
		if len(b) > 0 && b[len(b)-1] != '\n' {
			return false, errors.New("candidate log append ended with partial line")
		}
		scanner := bufio.NewScanner(bytes.NewReader(b))
		scanner.Buffer(make([]byte, 4096), 64<<10)
		for scanner.Scan() {
			selected, exact := selectedProxyRequestLine(scanner.Bytes(), agent, sessionID, markerHash, notBefore)
			if !selected {
				continue
			}
			if !exact || found {
				return false, errors.New("candidate log contains an unrelated or duplicate selected-session request")
			}
			found = true
		}
		if err := scanner.Err(); err != nil {
			return false, errors.New("candidate log line exceeded cap")
		}
	}
	return found, nil
}

func selectedProxyRequestLine(line []byte, agent, sessionID, markerHash string, notBefore time.Time) (bool, bool) {
	var object map[string]any
	if json.Unmarshal(line, &object) == nil {
		selected := object["msg"] == "proxy request" && object["agent"] == agent && object["session"] == sessionID
		if !selected {
			return false, false
		}
		observed, ok := parseLogTime(object["time"])
		return true, ok && !observed.Before(notBefore) && object["cutover_marker_hash"] == markerHash
	}
	if fields, observed, ok := parseDefaultSlogProxyRequest(string(line)); ok {
		selected := fields["agent"] == agent && fields["session"] == sessionID
		if !selected {
			return false, false
		}
		// The standard library default handler timestamps to whole seconds. The
		// file-identity snapshot and cryptographic marker still prove that this
		// line was appended by the challenged request, so compare at the
		// timestamp's actual precision instead of rejecting a same-second turn.
		return true, !observed.Before(notBefore.Truncate(time.Second)) && fields["cutover_marker_hash"] == markerHash
	}
	fields, ok := parseSlogText(string(line))
	if !ok {
		return false, false
	}
	selected := fields["msg"] == "proxy request" && fields["agent"] == agent && fields["session"] == sessionID
	if !selected {
		return false, false
	}
	observed, ok := parseLogTime(fields["time"])
	return true, ok && !observed.Before(notBefore) && fields["cutover_marker_hash"] == markerHash
}

func parseLogTime(value any) (time.Time, bool) {
	raw, ok := value.(string)
	if !ok {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	return parsed, err == nil
}

func parseDefaultSlogProxyRequest(line string) (map[string]string, time.Time, bool) {
	const (
		layout = "2006/01/02 15:04:05"
		prefix = " INFO proxy request "
	)
	if len(line) <= len(layout) || !strings.HasPrefix(line[len(layout):], prefix) {
		return nil, time.Time{}, false
	}
	observed, err := time.ParseInLocation(layout, line[:len(layout)], time.Local)
	if err != nil {
		return nil, time.Time{}, false
	}
	fields, ok := parseSlogText(line[len(layout)+len(prefix):])
	if !ok {
		return nil, time.Time{}, false
	}
	return fields, observed, true
}

func parseSlogText(line string) (map[string]string, bool) {
	fields := map[string]string{}
	for len(strings.TrimSpace(line)) > 0 {
		line = strings.TrimLeft(line, " \t")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return nil, false
		}
		key := line[:eq]
		line = line[eq+1:]
		var value string
		if strings.HasPrefix(line, "\"") {
			end := 1
			escaped := false
			for ; end < len(line); end++ {
				if escaped {
					escaped = false
					continue
				}
				if line[end] == '\\' {
					escaped = true
					continue
				}
				if line[end] == '"' {
					break
				}
			}
			if end >= len(line) {
				return nil, false
			}
			quoted := line[:end+1]
			decoded, err := strconv.Unquote(quoted)
			if err != nil {
				return nil, false
			}
			value = decoded
			line = line[end+1:]
		} else {
			end := strings.IndexAny(line, " \t")
			if end < 0 {
				value = line
				line = ""
			} else {
				value = line[:end]
				line = line[end:]
			}
		}
		if _, exists := fields[key]; exists {
			return nil, false
		}
		fields[key] = value
	}
	return fields, true
}
