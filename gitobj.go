package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
)

type ObjType string

const (
	ObjCommit ObjType = "commit"
	ObjTree   ObjType = "tree"
	ObjBlob   ObjType = "blob"
	ObjTag    ObjType = "tag"
)

type GitObj struct {
	Type ObjType
	Data []byte
}

const ZeroSha = "0000000000000000000000000000000000000000"

var typeCode = map[ObjType]int{
	ObjCommit: 1,
	ObjTree:   2,
	ObjBlob:   3,
	ObjTag:    4,
}

var codeType = map[int]ObjType{
	1: ObjCommit,
	2: ObjTree,
	3: ObjBlob,
	4: ObjTag,
}

func sha1Of(t ObjType, data []byte) string {
	h := sha1.New()
	header := fmt.Sprintf("%s %d\x00", t, len(data))
	h.Write([]byte(header))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func pktLine(payload []byte) []byte {
	length := len(payload) + 4
	hexLen := fmt.Sprintf("%04x", length)
	res := make([]byte, 4+len(payload))
	copy(res[0:4], hexLen)
	copy(res[4:], payload)
	return res
}

func pktLineStr(s string) []byte {
	return pktLine([]byte(s))
}

var PktFlush = []byte("0000")
var PktDelim = []byte("0001")

type Packet struct {
	Kind    int // 0=data, 1=flush, 2=delim, 3=end
	Payload []byte
}

func decodePkts(buf []byte, offset int) (packets []Packet, rest int) {
	i := offset
	for i+4 <= len(buf) {
		lenHex := string(buf[i : i+4])
		valid := true
		for _, c := range lenHex {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				valid = false
				break
			}
		}
		if !valid {
			break
		}
		length, err := strconv.ParseInt(lenHex, 16, 32)
		if err != nil {
			break
		}
		if length == 0 {
			packets = append(packets, Packet{Kind: 1, Payload: nil})
			i += 4
			break
		}
		if length == 1 {
			packets = append(packets, Packet{Kind: 2, Payload: nil})
			i += 4
			continue
		}
		if length == 2 {
			packets = append(packets, Packet{Kind: 3, Payload: nil})
			i += 4
			break
		}
		if length < 4 || i+int(length) > len(buf) {
			break
		}
		payload := buf[i+4 : i+int(length)]
		packets = append(packets, Packet{Kind: 0, Payload: payload})
		i += int(length)
	}
	return packets, i
}

func buildPack(objects []GitObj) []byte {
	var parts [][]byte
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	header[4] = 0
	header[5] = 0
	header[6] = 0
	header[7] = 2
	count := uint32(len(objects))
	header[8] = byte(count >> 24)
	header[9] = byte(count >> 16)
	header[10] = byte(count >> 8)
	header[11] = byte(count)
	parts = append(parts, header)

	for _, obj := range objects {
		size := len(obj.Data)
		tCode := typeCode[obj.Type]
		first := byte((tCode << 4) | (size & 0x0f))
		size >>= 4
		var varintBytes []byte
		for size > 0 {
			varintBytes = append(varintBytes, first|0x80)
			first = byte(size & 0x7f)
			size >>= 7
		}
		varintBytes = append(varintBytes, first)
		parts = append(parts, varintBytes)

		var deflated bytes.Buffer
		w, _ := zlib.NewWriterLevel(&deflated, flate.DefaultCompression)
		w.Write(obj.Data)
		w.Close()
		parts = append(parts, deflated.Bytes())
	}

	body := bytes.Join(parts, nil)
	h := sha1.New()
	h.Write(body)
	trailer := h.Sum(nil)

	return append(body, trailer...)
}

func applyDelta(base []byte, delta []byte) []byte {
	i := 0
	readVarint := func() int {
		v := 0
		shift := 0
		for {
			if i >= len(delta) {
				break
			}
			b := delta[i]
			i++
			v |= int(b&0x7f) << shift
			shift += 7
			if (b & 0x80) == 0 {
				break
			}
		}
		return v
	}

	readVarint() // source size
	targetSize := readVarint()
	out := make([]byte, targetSize)
	written := 0

	for i < len(delta) {
		op := delta[i]
		i++
		if (op & 0x80) != 0 {
			off := 0
			size := 0
			if (op & 0x01) != 0 {
				off = int(delta[i])
				i++
			}
			if (op & 0x02) != 0 {
				off |= int(delta[i]) << 8
				i++
			}
			if (op & 0x04) != 0 {
				off |= int(delta[i]) << 16
				i++
			}
			if (op & 0x08) != 0 {
				off |= int(delta[i]) << 24
				i++
			}
			if (op & 0x10) != 0 {
				size = int(delta[i])
				i++
			}
			if (op & 0x20) != 0 {
				size |= int(delta[i]) << 8
				i++
			}
			if (op & 0x40) != 0 {
				size |= int(delta[i]) << 16
				i++
			}
			if size == 0 {
				size = 0x10000
			}
			copy(out[written:written+size], base[off:off+size])
			written += size
		} else if op > 0 {
			copy(out[written:written+int(op)], delta[i:i+int(op)])
			i += int(op)
			written += int(op)
		} else {
			panic(fmt.Errorf("delta opcode 0"))
		}
	}
	return out
}

type ParsedPack struct {
	Objects map[string]GitObj
	Count   int
}

func inflateAt(buf []byte, offset int) ([]byte, int, error) {
	r := bytes.NewReader(buf[offset:])
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, 0, err
	}
	defer zr.Close()
	data, err := io.ReadAll(zr)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, 0, err
	}
	consumed := len(buf[offset:]) - r.Len()
	return data, offset + consumed, nil
}

type rawEntry struct {
	offset   int
	objType  ObjType
	data     []byte
	deltaOfs int
	deltaRef string
	rawDelta []byte
}

func parsePack(buf []byte, external func(sha string) *GitObj) (ParsedPack, error) {
	if len(buf) < 12 || string(buf[0:4]) != "PACK" {
		return ParsedPack{}, fmt.Errorf("bad pack header")
	}
	count := int(buf[8])<<24 | int(buf[9])<<16 | int(buf[10])<<8 | int(buf[11])
	i := 12

	var raw []rawEntry
	byOffset := make(map[int]int)

	for n := 0; n < count; n++ {
		start := i
		if i >= len(buf) {
			return ParsedPack{}, fmt.Errorf("unexpected EOF in pack")
		}
		b := buf[i]
		i++
		typeCodeNum := (b >> 4) & 7
		size := int(b & 0x0f)
		shift := 4
		for (b & 0x80) != 0 {
			if i >= len(buf) {
				return ParsedPack{}, fmt.Errorf("unexpected EOF in pack varint")
			}
			b = buf[i]
			i++
			size |= int(b&0x7f) << shift
			shift += 7
		}

		if typeCodeNum == 6 {
			if i >= len(buf) {
				return ParsedPack{}, fmt.Errorf("unexpected EOF in ofs-delta")
			}
			c := buf[i]
			i++
			off := int(c & 0x7f)
			for (c & 0x80) != 0 {
				if i >= len(buf) {
					return ParsedPack{}, fmt.Errorf("unexpected EOF in ofs-delta continuation")
				}
				c = buf[i]
				i++
				off = ((off + 1) << 7) | int(c&0x7f)
			}
			inflated, nextOffset, err := inflateAt(buf, i)
			if err != nil {
				return ParsedPack{}, err
			}
			raw = append(raw, rawEntry{
				offset:   start,
				objType:  ObjBlob,
				deltaOfs: start - off,
				rawDelta: inflated,
			})
			i = nextOffset
		} else if typeCodeNum == 7 {
			if i+20 > len(buf) {
				return ParsedPack{}, fmt.Errorf("unexpected EOF in ref-delta sha")
			}
			ref := hex.EncodeToString(buf[i : i+20])
			i += 20
			inflated, nextOffset, err := inflateAt(buf, i)
			if err != nil {
				return ParsedPack{}, err
			}
			raw = append(raw, rawEntry{
				offset:   start,
				objType:  ObjBlob,
				deltaRef: ref,
				rawDelta: inflated,
			})
			i = nextOffset
		} else {
			t, ok := codeType[int(typeCodeNum)]
			if !ok {
				return ParsedPack{}, fmt.Errorf("unknown object type code %d", typeCodeNum)
			}
			inflated, nextOffset, err := inflateAt(buf, i)
			if err != nil {
				return ParsedPack{}, err
			}
			raw = append(raw, rawEntry{
				offset:  start,
				objType: t,
				data:    inflated,
			})
			i = nextOffset
		}
		byOffset[start] = len(raw) - 1
	}

	resolved := make(map[string]GitObj)
	memo := make(map[int]GitObj)

	var resolveIdx func(idx int) (GitObj, error)
	resolveIdx = func(idx int) (GitObj, error) {
		if obj, ok := memo[idx]; ok {
			return obj, nil
		}
		entry := raw[idx]
		var obj GitObj
		if entry.data != nil && entry.rawDelta == nil {
			obj = GitObj{Type: entry.objType, Data: entry.data}
		} else {
			var base GitObj
			var err error
			if entry.deltaOfs != 0 {
				baseIdx, ok := byOffset[entry.deltaOfs]
				if !ok {
					return GitObj{}, fmt.Errorf("ofs-delta base at offset %d not found", entry.deltaOfs)
				}
				base, err = resolveIdx(baseIdx)
				if err != nil {
					return GitObj{}, err
				}
			} else if entry.deltaRef != "" {
				found := -1
				for j := 0; j < len(raw); j++ {
					r := raw[j]
					if r.data != nil && r.rawDelta == nil && sha1Of(r.objType, r.data) == entry.deltaRef {
						found = j
						break
					}
				}
				if found >= 0 {
					base, err = resolveIdx(found)
					if err != nil {
						return GitObj{}, err
					}
				} else {
					if external != nil {
						ext := external(entry.deltaRef)
						if ext == nil {
							return GitObj{}, fmt.Errorf("delta base missing for ref %s", entry.deltaRef)
						}
						base = *ext
					} else {
						return GitObj{}, fmt.Errorf("delta base missing for ref %s", entry.deltaRef)
					}
				}
			} else {
				return GitObj{}, fmt.Errorf("invalid delta entry without ofs or ref")
			}
			obj = GitObj{Type: base.Type, Data: applyDelta(base.Data, entry.rawDelta)}
		}
		memo[idx] = obj
		return obj, nil
	}

	for idx := 0; idx < len(raw); idx++ {
		obj, err := resolveIdx(idx)
		if err != nil {
			return ParsedPack{}, err
		}
		resolved[sha1Of(obj.Type, obj.Data)] = obj
	}

	return ParsedPack{Objects: resolved, Count: count}, nil
}
