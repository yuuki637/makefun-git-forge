package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/adler32"
	"io"
	"sort"
	"strings"
	"sync"
)

// Git object model, packfile read/write, pkt-line framing. Stdlib only.
// Ported from the Node implementation (git.ts) with identical wire behavior.

type ObjType string

const (
	ObjCommit ObjType = "commit"
	ObjTree   ObjType = "tree"
	ObjBlob   ObjType = "blob"
	ObjTag    ObjType = "tag"
)

type GitObject struct {
	Type ObjType
	Data []byte

	headerOnce sync.Once
	header     CommitHeader
}

const ZeroOID = "0000000000000000000000000000000000000000"

var typeIDs = map[ObjType]int{ObjCommit: 1, ObjTree: 2, ObjBlob: 3, ObjTag: 4}
var typeNames = map[int]ObjType{1: ObjCommit, 2: ObjTree, 3: ObjBlob, 4: ObjTag}

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

func hashObject(t ObjType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", t, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------- pkt-lines
func pktLine(payload []byte) []byte {
	total := len(payload) + 4
	if total > 65520 {
		panic("pkt-line too large")
	}
	out := make([]byte, 0, total)
	out = append(out, []byte(fmt.Sprintf("%04x", total))...)
	out = append(out, payload...)
	return out
}

func pktLineStr(s string) []byte { return pktLine([]byte(s)) }

var (
	pktFlush       = []byte("0000")
	pktDelim       = []byte("0001")
	pktResponseEnd = []byte("0002")
)

type Packet struct {
	Kind string // data | flush | delim | response-end
	Data []byte
}

func isHex4(b []byte) bool {
	for _, c := range b {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// Stream decoder over a complete buffer: returns packets and unconsumed rest.
func decodePackets(buffer []byte, start int) ([]Packet, []byte) {
	var packets []Packet
	offset := start
	for offset < len(buffer) {
		if len(buffer)-offset < 4 {
			break
		}
		hexPart := buffer[offset : offset+4]
		if !isHex4(hexPart) {
			break
		}
		var length int
		fmt.Sscanf(string(hexPart), "%x", &length)
		if length == 0 {
			packets = append(packets, Packet{Kind: "flush"})
			offset += 4
			continue
		}
		if length == 1 {
			packets = append(packets, Packet{Kind: "delim"})
			offset += 4
			continue
		}
		if length == 2 {
			packets = append(packets, Packet{Kind: "response-end"})
			offset += 4
			continue
		}
		if length < 4 || offset+length > len(buffer) {
			break
		}
		packets = append(packets, Packet{Kind: "data", Data: buffer[offset+4 : offset+length]})
		offset += length
	}
	return packets, buffer[offset:]
}

// ------------------------------------------------------------------- objects
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func parseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], ' ')
		if space < 0 {
			break
		}
		space += offset
		mode := string(data[offset:space])
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			break
		}
		nul += space + 1
		name := string(data[space+1 : nul])
		if nul+21 > len(data) {
			break
		}
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		offset = nul + 21
	}
	return entries
}

func treeSortKey(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

func buildTreeData(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return treeSortKey(sorted[i]) < treeSortKey(sorted[j]) })
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

type CommitData struct {
	Tree           string
	Parents        []string
	AuthorLine     string
	CommitterLine  string
	AuthorName     string
	AuthorEmail    string
	CommitterName  string
	CommitterEmail string
	Message        string
}

// parseNameEmail mirrors the JS regex /^(.*) <(.*)>/ (greedy first group).
func parseNameEmail(line string) (string, string, bool) {
	open := strings.LastIndex(line, " <")
	if open < 0 || !strings.HasSuffix(line, ">") {
		return "", "", false
	}
	return line[:open], line[open+2 : len(line)-1], true
}

func parseCommit(data []byte) CommitData {
	text := string(data)
	split := strings.Index(text, "\n\n")
	header := text
	message := ""
	if split != -1 {
		header = text[:split]
		message = text[split+2:]
	}
	result := CommitData{Tree: ZeroOID, Message: message}
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			result.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			result.Parents = append(result.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			result.AuthorLine = line[7:]
			if n, e, ok := parseNameEmail(result.AuthorLine); ok {
				result.AuthorName, result.AuthorEmail = n, e
			}
		case strings.HasPrefix(line, "committer "):
			result.CommitterLine = line[10:]
			if n, e, ok := parseNameEmail(result.CommitterLine); ok {
				result.CommitterName, result.CommitterEmail = n, e
			}
		}
	}
	return result
}

type CommitHeader struct {
	Tree    string
	Parents []string
}

// Byte-level tree+parents scan (mirrors parseCommitHeader in git.ts).
func parseCommitHeader(data []byte) CommitHeader {
	tree := ZeroOID
	var parents []string
	pos := 0
	for pos < len(data) {
		nl := bytes.IndexByte(data[pos:], '\n')
		var lineEnd int
		if nl == -1 {
			lineEnd = len(data)
		} else {
			lineEnd = pos + nl
		}
		if lineEnd == pos {
			break
		}
		if lineEnd-pos >= 45 && data[pos] == 't' && string(data[pos:pos+5]) == "tree " {
			tree = string(data[pos+5 : pos+45])
		} else if lineEnd-pos >= 47 && data[pos] == 'p' && string(data[pos:pos+7]) == "parent " {
			parents = append(parents, string(data[pos+7:pos+47]))
		}
		if nl == -1 {
			break
		}
		pos = pos + nl + 1
	}
	return CommitHeader{Tree: tree, Parents: parents}
}

// Commit objects are immutable once stored; cache the parsed header on the
// object itself.
func commitHeaderOf(obj *GitObject) CommitHeader {
	obj.headerOnce.Do(func() {
		obj.header = parseCommitHeader(obj.Data)
	})
	return obj.header
}

func buildCommitData(tree string, parents []string, authorName, authorEmail, committerName, committerEmail string, timestamp int64, message string) []byte {
	author := fmt.Sprintf("%s <%s> %d +0000", authorName, authorEmail, timestamp)
	committer := fmt.Sprintf("%s <%s> %d +0000", committerName, committerEmail, timestamp)
	var sb strings.Builder
	sb.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		sb.WriteString("parent " + p + "\n")
	}
	sb.WriteString("author " + author + "\ncommitter " + committer + "\n\n" + message)
	return []byte(sb.String())
}

type TagData struct {
	Object  string
	Type    string
	Tag     string
	Message string
}

func parseTag(data []byte) TagData {
	text := string(data)
	split := strings.Index(text, "\n\n")
	header := text
	result := TagData{Object: ZeroOID}
	if split != -1 {
		header = text[:split]
		result.Message = text[split+2:]
	}
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			result.Object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			result.Type = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "tag "):
			result.Tag = strings.TrimSpace(line[4:])
		}
	}
	return result
}

// ---------------------------------------------------------------- pack write
type packEntry struct {
	OID  string
	Type ObjType
	Data []byte
}

func writePack(entries []packEntry) []byte {
	var buf bytes.Buffer
	header := make([]byte, 12)
	copy(header, "PACK")
	binary.BigEndian.PutUint32(header[4:], 2)
	binary.BigEndian.PutUint32(header[8:], uint32(len(entries)))
	buf.Write(header)
	for _, entry := range entries {
		size := len(entry.Data)
		typeID := typeIDs[entry.Type]
		first := (typeID << 4) | (size & 0x0f)
		rest := size >> 4
		if rest > 0 {
			first |= 0x80
		}
		buf.WriteByte(byte(first))
		for rest > 0 {
			b := rest & 0x7f
			rest >>= 7
			if rest > 0 {
				b |= 0x80
			}
			buf.WriteByte(byte(b))
		}
		zw, _ := zlib.NewWriterLevelDict(&buf, zlib.DefaultCompression, nil)
		zw.Write(entry.Data)
		zw.Close()
	}
	body := buf.Bytes()
	trailer := sha1.Sum(body)
	return append(body, trailer[:]...)
}

// ----------------------------------------------------------------- pack read
func applyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readVarint := func() int {
		value := 0
		shift := 0
		for {
			b := delta[offset]
			offset++
			value |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value
	}
	sourceSize := readVarint()
	if sourceSize != len(base) {
		return nil, errors.New("delta source size mismatch")
	}
	targetSize := readVarint()
	var out bytes.Buffer
	produced := 0
	for offset < len(delta) {
		opcode := delta[offset]
		offset++
		if opcode&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			if opcode&0x01 != 0 {
				copyOffset = int(delta[offset])
				offset++
			}
			if opcode&0x02 != 0 {
				copyOffset |= int(delta[offset]) << 8
				offset++
			}
			if opcode&0x04 != 0 {
				copyOffset |= int(delta[offset]) << 16
				offset++
			}
			if opcode&0x08 != 0 {
				copyOffset |= int(delta[offset]) << 24
				offset++
			}
			if opcode&0x10 != 0 {
				copySize = int(delta[offset])
				offset++
			}
			if opcode&0x20 != 0 {
				copySize |= int(delta[offset]) << 8
				offset++
			}
			if opcode&0x40 != 0 {
				copySize |= int(delta[offset]) << 16
				offset++
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			out.Write(base[copyOffset : copyOffset+copySize])
			produced += copySize
		} else if opcode > 0 {
			out.Write(delta[offset : offset+int(opcode)])
			offset += int(opcode)
			produced += int(opcode)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	result := out.Bytes()
	if len(result) != targetSize || produced != targetSize {
		return nil, errors.New("delta target size mismatch")
	}
	return result, nil
}

func inflateWhole(data []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// The original boundary search, kept as a fallback (mirrors git.ts).
func inflateBinarySearch(buffer []byte, offset int) ([]byte, int, error) {
	best := -1
	loBound := 1
	hiBound := len(buffer) - offset
	for loBound <= hiBound {
		mid := (loBound + hiBound) >> 1
		if _, err := inflateWhole(buffer[offset : offset+mid]); err == nil {
			best = mid
			hiBound = mid - 1
		} else {
			loBound = mid + 1
		}
	}
	if best == -1 {
		return nil, 0, errors.New("cannot inflate pack object")
	}
	data, err := inflateWhole(buffer[offset : offset+best])
	if err != nil {
		return nil, 0, err
	}
	return data, best, nil
}

// Inflates one object body starting at offset; returns bytes consumed.
// Fast path mirrors git.ts: inflate the remainder once, then locate the
// stream end via the adler32 trailer of the plain data.
func inflateAt(buffer []byte, offset int, expectedSize int) ([]byte, int, error) {
	data, err := inflateWhole(buffer[offset:])
	if err == nil && len(data) == expectedSize {
		sum := adler32.Checksum(data)
		var trailer [4]byte
		binary.BigEndian.PutUint32(trailer[:], sum)
		searchFrom := offset + 4
		for searchFrom < len(buffer) {
			idx := bytes.Index(buffer[searchFrom:], trailer[:])
			if idx == -1 {
				break
			}
			abs := searchFrom + idx
			end := abs + 4
			if candidate, err := inflateWhole(buffer[offset:end]); err == nil && bytes.Equal(candidate, data) {
				return data, end - offset, nil
			}
			searchFrom = abs + 1
		}
	}
	return inflateBinarySearch(buffer, offset)
}

// Parses a pack buffer. resolveBase supplies objects not in the pack.
func readPack(buffer []byte, resolveBase func(oid string) *GitObject) (map[string]*GitObject, error) {
	if len(buffer) < 12+20 {
		return nil, errors.New("pack too small")
	}
	if string(buffer[0:4]) != "PACK" {
		return nil, errors.New("bad pack signature")
	}
	version := binary.BigEndian.Uint32(buffer[4:])
	if version != 2 && version != 3 {
		return nil, errors.New("bad pack version")
	}
	count := int(binary.BigEndian.Uint32(buffer[8:]))
	trailer := buffer[len(buffer)-20:]
	checksum := sha1.Sum(buffer[:len(buffer)-20])
	if !bytes.Equal(checksum[:], trailer) {
		return nil, errors.New("pack checksum mismatch")
	}

	type rawObj struct {
		typeID      int
		data        []byte
		baseOID     string
		baseOffset  int
		hasBaseOff  bool
		startOffset int
	}
	var raws []*rawObj
	offset := 12
	for i := 0; i < count; i++ {
		startOffset := offset
		b := buffer[offset]
		offset++
		typeID := int(b>>4) & 0x07
		size := int(b & 0x0f)
		shift := 4
		for b&0x80 != 0 {
			b = buffer[offset]
			offset++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		raw := &rawObj{typeID: typeID, startOffset: startOffset}
		if typeID == 6 {
			b = buffer[offset]
			offset++
			off := int(b & 0x7f)
			for b&0x80 != 0 {
				b = buffer[offset]
				offset++
				off = ((off + 1) << 7) | int(b&0x7f)
			}
			raw.baseOffset = startOffset - off
			raw.hasBaseOff = true
		} else if typeID == 7 {
			raw.baseOID = hex.EncodeToString(buffer[offset : offset+20])
			offset += 20
		}
		data, consumed, err := inflateAt(buffer, offset, size)
		if err != nil {
			return nil, err
		}
		offset += consumed
		raw.data = data
		raws = append(raws, raw)
	}

	byOffset := map[int]int{}
	for idx, raw := range raws {
		byOffset[raw.startOffset] = idx
	}
	resolved := map[int]*GitObject{}
	result := map[string]*GitObject{}

	var resolveIndex func(idx int) (*GitObject, error)
	resolveIndex = func(idx int) (*GitObject, error) {
		if cached, ok := resolved[idx]; ok {
			return cached, nil
		}
		raw := raws[idx]
		var obj *GitObject
		if raw.typeID >= 1 && raw.typeID <= 4 {
			obj = &GitObject{Type: typeNames[raw.typeID], Data: raw.data}
		} else if raw.typeID == 6 || raw.typeID == 7 {
			var base *GitObject
			if raw.typeID == 6 {
				if baseIdx, ok := byOffset[raw.baseOffset]; ok {
					b, err := resolveIndex(baseIdx)
					if err != nil {
						return nil, err
					}
					base = b
				}
			} else {
				if inPack, ok := result[raw.baseOID]; ok {
					base = inPack
				} else {
					base = resolveBase(raw.baseOID)
				}
			}
			if base == nil {
				return nil, errors.New("delta base not found")
			}
			data, err := applyDelta(base.Data, raw.data)
			if err != nil {
				return nil, err
			}
			obj = &GitObject{Type: base.Type, Data: data}
		} else {
			return nil, fmt.Errorf("unknown pack object type %d", raw.typeID)
		}
		resolved[idx] = obj
		result[hashObject(obj.Type, obj.Data)] = obj
		return obj, nil
	}
	for i := range raws {
		if _, err := resolveIndex(i); err != nil {
			return nil, err
		}
	}
	return result, nil
}
