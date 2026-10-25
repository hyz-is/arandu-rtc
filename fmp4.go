package rtc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Track is what a decoder needs to know before the first picture: the codec, the picture size,
// and the codec's own configuration record exactly as the container carries it.
type Track struct {
	// Codec is the string a browser's VideoDecoder and MediaSource.isTypeSupported take, such as
	// avc1.640028 or hvc1.1.6.L93.B0.
	Codec string
	// Width and Height are the coded picture's size in pixels.
	Width, Height int
	// Description is the avcC or hvcC record. A decoder configured with it expects every sample
	// as length-prefixed NAL units, which is what a sample is.
	Description []byte
	// Timescale is the ticks per second of the sample times in the stream.
	Timescale uint32
}

// Sample is one coded picture, as the camera sent it: no decoding and no re-encoding happened to
// get here.
type Sample struct {
	// Key is true when decoding can start at this sample.
	Key bool
	// PTS is the presentation time, from the start of the stream.
	PTS time.Duration
	// Wall is when the node had the sample. It is what an overlay is aligned to: a detection
	// carries the wall time of the frame it was found on.
	Wall time.Time
	// Data is the coded picture in length-prefixed NAL units. It is never written after the
	// Sample is made, so any number of viewers may hold it.
	Data []byte
}

// Demuxer reads the fragmented MP4 that ffmpeg writes with -c copy -movflags
// empty_moov+default_base_moof+frag_keyframe, as it arrives, and hands out the track once and each
// sample after it.
//
// It is the one place that knows the container. It reads boxes, not bytes of a file: nothing here
// seeks, and a stream that starts again (a camera that reconnected) is a new Demuxer.
//
// It is not safe for concurrent use. A Write that returns an error leaves it unusable; the caller
// keeps the pipe drained and discards what it reads.
type Demuxer struct {
	onTrack  func(Track)
	onSample func(Sample)
	onBox    func(kind string, box []byte)
	now      func() time.Time

	buf []byte

	track   *Track
	trackID uint32
	// Defaults from the movie extends box, for a fragment whose header names none.
	defDuration, defSize, defFlags uint32

	// A fragment header waits here for the data box that follows it.
	moofLen int
	run     []sampleRun
}

type sampleRun struct {
	offset   int // from the start of the fragment's header box
	size     int
	duration uint32
	flags    uint32
	cts      int64
	dts      uint64
}

// Limits on what one box may ask this package to hold. A camera's keyframe is a few hundred
// kilobytes; a box announcing more than this is not from one.
const (
	maxBox     = 32 << 20
	maxSamples = 1 << 16
)

// ErrContainer is a stream that is not the fragmented MP4 this package reads.
var ErrContainer = errors.New("live: the stream is not fragmented MP4 with one video track")

// NewDemuxer returns a Demuxer calling onTrack when the stream's header is complete, and onSample
// for each sample in order. Either callback runs on the goroutine that called Write.
func NewDemuxer(onTrack func(Track), onSample func(Sample)) *Demuxer {
	return &Demuxer{onTrack: onTrack, onSample: onSample, now: time.Now}
}

// Boxes asks for every top-level box as it is read whole, header included, after what it holds has
// been handed out. The bytes are the demuxer's own and are valid for the call only: whoever keeps
// them copies. It is how a stream is kept as it came, to be handed on to something that wants the
// container and not the pictures.
func (d *Demuxer) Boxes(fn func(kind string, box []byte)) { d.onBox = fn }

// Write takes the next bytes of the stream.
func (d *Demuxer) Write(p []byte) (int, error) {
	d.buf = append(d.buf, p...)
	for {
		consumed, err := d.box()
		if err != nil {
			return len(p), err
		}
		if consumed == 0 {
			break
		}
		d.buf = d.buf[consumed:]
	}
	// Do not keep a large backing array alive for the rest of the stream.
	if len(d.buf) == 0 && cap(d.buf) > 1<<20 {
		d.buf = nil
	}
	return len(p), nil
}

// box handles the first box of the buffer when it is whole, and says how many bytes it was.
func (d *Demuxer) box() (int, error) {
	if len(d.buf) < 8 {
		return 0, nil
	}
	size := uint64(binary.BigEndian.Uint32(d.buf))
	kind := string(d.buf[4:8])
	header := 8
	switch size {
	case 0:
		return 0, fmt.Errorf("%w: a %q box runs to the end of the stream", ErrContainer, kind)
	case 1:
		if len(d.buf) < 16 {
			return 0, nil
		}
		size = binary.BigEndian.Uint64(d.buf[8:])
		header = 16
	}
	if size < uint64(header) || size > maxBox {
		return 0, fmt.Errorf("%w: a %q box of %d bytes", ErrContainer, kind, size)
	}
	if uint64(len(d.buf)) < size {
		return 0, nil
	}
	body := d.buf[header:size]
	switch kind {
	case "moov":
		if err := d.movie(body); err != nil {
			return 0, err
		}
	case "moof":
		if d.track == nil {
			return 0, fmt.Errorf("%w: a fragment before the header", ErrContainer)
		}
		if err := d.fragment(body, int(size)); err != nil {
			return 0, err
		}
	case "mdat":
		if err := d.data(body); err != nil {
			return 0, err
		}
	}
	if d.onBox != nil {
		d.onBox(kind, d.buf[:size])
	}
	return int(size), nil
}

// children calls fn for each box inside body.
func children(body []byte, fn func(kind string, inner []byte) error) error {
	for len(body) > 0 {
		if len(body) < 8 {
			return fmt.Errorf("%w: a box header cut short", ErrContainer)
		}
		size := uint64(binary.BigEndian.Uint32(body))
		header := 8
		if size == 1 {
			if len(body) < 16 {
				return fmt.Errorf("%w: a box header cut short", ErrContainer)
			}
			size, header = binary.BigEndian.Uint64(body[8:]), 16
		} else if size == 0 {
			size = uint64(len(body))
		}
		if size < uint64(header) || size > uint64(len(body)) {
			return fmt.Errorf("%w: a box of %d bytes inside %d", ErrContainer, size, len(body))
		}
		if err := fn(string(body[4:8]), body[header:size]); err != nil {
			return err
		}
		body = body[size:]
	}
	return nil
}

func (d *Demuxer) movie(moov []byte) error {
	if d.track != nil {
		return nil // the same header again, as a second segment would repeat it
	}
	var found *Track
	var id uint32
	err := children(moov, func(kind string, inner []byte) error {
		switch kind {
		case "trak":
			if found != nil {
				return nil
			}
			t, trackID, ok, err := parseTrack(inner)
			if err != nil {
				return err
			}
			if ok {
				found, id = &t, trackID
			}
		case "mvex":
			return children(inner, func(kind string, inner []byte) error {
				if kind == "trex" && len(inner) >= 24 {
					d.defDuration = binary.BigEndian.Uint32(inner[12:])
					d.defSize = binary.BigEndian.Uint32(inner[16:])
					d.defFlags = binary.BigEndian.Uint32(inner[20:])
				}
				return nil
			})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if found == nil {
		return fmt.Errorf("%w: no H.264 or H.265 track", ErrContainer)
	}
	d.track, d.trackID = found, id
	if d.onTrack != nil {
		t := *found
		t.Description = append([]byte(nil), found.Description...)
		d.onTrack(t)
	}
	return nil
}

// parseTrack reads one trak box, and says whether it is a video track this package carries.
func parseTrack(trak []byte) (Track, uint32, bool, error) {
	var (
		t       Track
		id      uint32
		isVideo bool
	)
	err := children(trak, func(kind string, inner []byte) error {
		switch kind {
		case "tkhd":
			if len(inner) >= 16 {
				if inner[0] == 1 {
					if len(inner) >= 24 {
						id = binary.BigEndian.Uint32(inner[20:])
					}
				} else {
					id = binary.BigEndian.Uint32(inner[12:])
				}
			}
		case "mdia":
			return children(inner, func(kind string, inner []byte) error {
				switch kind {
				case "mdhd":
					if len(inner) >= 24 {
						if inner[0] == 1 {
							t.Timescale = binary.BigEndian.Uint32(inner[20:])
						} else {
							t.Timescale = binary.BigEndian.Uint32(inner[12:])
						}
					}
				case "minf":
					return children(inner, func(kind string, inner []byte) error {
						if kind != "stbl" {
							return nil
						}
						return children(inner, func(kind string, inner []byte) error {
							if kind != "stsd" || len(inner) < 8 {
								return nil
							}
							ok, err := parseSampleEntry(inner[8:], &t)
							isVideo = isVideo || ok
							return err
						})
					})
				}
				return nil
			})
		}
		return nil
	})
	if err != nil {
		return Track{}, 0, false, err
	}
	if !isVideo {
		return Track{}, 0, false, nil
	}
	if t.Timescale == 0 {
		return Track{}, 0, false, fmt.Errorf("%w: the video track has no timescale", ErrContainer)
	}
	return t, id, true, nil
}

// parseSampleEntry reads the first sample entry of a sample description, when it is H.264 or
// H.265.
func parseSampleEntry(entries []byte, t *Track) (bool, error) {
	ok := false
	err := children(entries, func(kind string, inner []byte) error {
		if ok {
			return nil
		}
		switch kind {
		case "avc1", "avc3", "hvc1", "hev1":
		default:
			return nil
		}
		// A visual sample entry: 6 reserved bytes, the data reference, 16 bytes, the size, then
		// 50 more before the boxes that carry the codec's configuration.
		if len(inner) < 78 {
			return fmt.Errorf("%w: a %q entry of %d bytes", ErrContainer, kind, len(inner))
		}
		t.Width = int(binary.BigEndian.Uint16(inner[24:]))
		t.Height = int(binary.BigEndian.Uint16(inner[26:]))
		return children(inner[78:], func(box string, config []byte) error {
			switch {
			case (kind == "avc1" || kind == "avc3") && box == "avcC":
				codec, err := avcCodec(kind, config)
				if err != nil {
					return err
				}
				t.Codec, t.Description, ok = codec, append([]byte(nil), config...), true
			case (kind == "hvc1" || kind == "hev1") && box == "hvcC":
				codec, err := hevcCodec(kind, config)
				if err != nil {
					return err
				}
				t.Codec, t.Description, ok = codec, append([]byte(nil), config...), true
			}
			return nil
		})
	})
	return ok, err
}

// avcCodec is the RFC 6381 string of an AVCDecoderConfigurationRecord.
func avcCodec(entry string, record []byte) (string, error) {
	if len(record) < 7 || record[0] != 1 {
		return "", fmt.Errorf("%w: an avcC record of %d bytes", ErrContainer, len(record))
	}
	return fmt.Sprintf("%s.%02X%02X%02X", entry, record[1], record[2], record[3]), nil
}

// hevcCodec is the string of an HEVCDecoderConfigurationRecord, as ISO 14496-15 Annex E writes it.
func hevcCodec(entry string, record []byte) (string, error) {
	if len(record) < 13 || record[0] != 1 {
		return "", fmt.Errorf("%w: an hvcC record of %d bytes", ErrContainer, len(record))
	}
	space := ""
	switch record[1] >> 6 {
	case 1:
		space = "A"
	case 2:
		space = "B"
	case 3:
		space = "C"
	}
	tier := "L"
	if record[1]&0x20 != 0 {
		tier = "H"
	}
	// The 32 compatibility flags are written in reverse bit order.
	compat := binary.BigEndian.Uint32(record[2:6])
	var reversed uint32
	for i := 0; i < 32; i++ {
		reversed |= (compat >> i & 1) << (31 - i)
	}
	constraints := record[6:12]
	last := len(constraints)
	for last > 0 && constraints[last-1] == 0 {
		last--
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s.%s%d.%X.%s%d", entry, space, record[1]&0x1f, reversed, tier, record[12])
	for _, c := range constraints[:last] {
		fmt.Fprintf(&b, ".%X", c)
	}
	return b.String(), nil
}

// fragment reads a moof box into the runs of samples its data box will fill.
func (d *Demuxer) fragment(moof []byte, boxLen int) error {
	d.run, d.moofLen = d.run[:0], boxLen
	return children(moof, func(kind string, traf []byte) error {
		if kind != "traf" {
			return nil
		}
		return d.trackFragment(traf)
	})
}

// Flags of the fragment boxes.
const (
	tfhdBaseOffset      = 0x000001
	tfhdSampleDesc      = 0x000002
	tfhdDefaultDuration = 0x000008
	tfhdDefaultSize     = 0x000010
	tfhdDefaultFlags    = 0x000020

	trunDataOffset  = 0x000001
	trunFirstFlags  = 0x000004
	trunDuration    = 0x000100
	trunSize        = 0x000200
	trunFlags       = 0x000400
	trunCompOffsets = 0x000800

	// A sample that is not a sync sample carries this bit.
	sampleNotSync = 0x00010000
)

func (d *Demuxer) trackFragment(traf []byte) error {
	var (
		id                             uint32
		defDuration, defSize, defFlags = d.defDuration, d.defSize, d.defFlags
		baseTime                       uint64
		belongs                        = true
	)
	return children(traf, func(kind string, b []byte) error {
		switch kind {
		case "tfhd":
			if len(b) < 8 {
				return fmt.Errorf("%w: a tfhd of %d bytes", ErrContainer, len(b))
			}
			flags := binary.BigEndian.Uint32(b) & 0xffffff
			id = binary.BigEndian.Uint32(b[4:])
			belongs = id == d.trackID
			p := 8
			read := func(present uint32, into *uint32, width int) error {
				if flags&present == 0 {
					return nil
				}
				if len(b) < p+width {
					return fmt.Errorf("%w: a tfhd cut short", ErrContainer)
				}
				if into != nil {
					*into = binary.BigEndian.Uint32(b[p:])
				}
				p += width
				return nil
			}
			if err := read(tfhdBaseOffset, nil, 8); err != nil {
				return err
			}
			if err := read(tfhdSampleDesc, nil, 4); err != nil {
				return err
			}
			if err := read(tfhdDefaultDuration, &defDuration, 4); err != nil {
				return err
			}
			if err := read(tfhdDefaultSize, &defSize, 4); err != nil {
				return err
			}
			return read(tfhdDefaultFlags, &defFlags, 4)
		case "tfdt":
			if len(b) < 8 {
				return fmt.Errorf("%w: a tfdt of %d bytes", ErrContainer, len(b))
			}
			if b[0] == 1 {
				if len(b) < 12 {
					return fmt.Errorf("%w: a tfdt of %d bytes", ErrContainer, len(b))
				}
				baseTime = binary.BigEndian.Uint64(b[4:])
			} else {
				baseTime = uint64(binary.BigEndian.Uint32(b[4:]))
			}
		case "trun":
			if !belongs {
				return nil
			}
			return d.trackRun(b, defDuration, defSize, defFlags, baseTime)
		}
		return nil
	})
}

func (d *Demuxer) trackRun(b []byte, defDuration, defSize, defFlags uint32, baseTime uint64) error {
	if len(b) < 8 {
		return fmt.Errorf("%w: a trun of %d bytes", ErrContainer, len(b))
	}
	version := b[0]
	flags := binary.BigEndian.Uint32(b) & 0xffffff
	count := int(binary.BigEndian.Uint32(b[4:]))
	if count > maxSamples {
		return fmt.Errorf("%w: a run of %d samples", ErrContainer, count)
	}
	p := 8
	offset := 0
	if flags&trunDataOffset != 0 {
		if len(b) < p+4 {
			return fmt.Errorf("%w: a trun cut short", ErrContainer)
		}
		offset = int(int32(binary.BigEndian.Uint32(b[p:])))
		p += 4
	}
	first := defFlags
	hasFirst := flags&trunFirstFlags != 0
	if hasFirst {
		if len(b) < p+4 {
			return fmt.Errorf("%w: a trun cut short", ErrContainer)
		}
		first = binary.BigEndian.Uint32(b[p:])
		p += 4
	}
	dts := baseTime
	for i := 0; i < count; i++ {
		s := sampleRun{offset: offset, duration: defDuration, size: int(defSize), flags: defFlags, dts: dts}
		if i == 0 && hasFirst {
			s.flags = first
		}
		take := func(present uint32) (uint32, bool, error) {
			if flags&present == 0 {
				return 0, false, nil
			}
			if len(b) < p+4 {
				return 0, false, fmt.Errorf("%w: a trun cut short", ErrContainer)
			}
			v := binary.BigEndian.Uint32(b[p:])
			p += 4
			return v, true, nil
		}
		if v, ok, err := take(trunDuration); err != nil {
			return err
		} else if ok {
			s.duration = v
		}
		if v, ok, err := take(trunSize); err != nil {
			return err
		} else if ok {
			s.size = int(v)
		}
		if v, ok, err := take(trunFlags); err != nil {
			return err
		} else if ok {
			s.flags = v
		}
		if v, ok, err := take(trunCompOffsets); err != nil {
			return err
		} else if ok {
			if version == 0 {
				s.cts = int64(v)
			} else {
				s.cts = int64(int32(v))
			}
		}
		if s.size < 0 || s.size > maxBox {
			return fmt.Errorf("%w: a sample of %d bytes", ErrContainer, s.size)
		}
		d.run = append(d.run, s)
		offset += s.size
		dts += uint64(s.duration)
	}
	return nil
}

// data cuts the data box into the samples the fragment header announced.
//
// The fragment reaches the node when its last sample is complete, so that sample's wall time is
// now and each earlier one is older by the picture time between them: a viewer aligning an
// overlay to a sample would otherwise see a burst of samples with one time.
func (d *Demuxer) data(mdat []byte) error {
	run := d.run
	d.run = d.run[:0]
	if len(run) == 0 {
		return nil
	}
	now := d.now()
	pts := func(s sampleRun) int64 {
		if v := int64(s.dts) + s.cts; v > 0 {
			return v
		}
		return 0
	}
	last := pts(run[len(run)-1])
	scale := time.Duration(d.track.Timescale)
	for _, s := range run {
		// A sample's offset counts from the fragment header's first byte, and the data box
		// follows the header directly, behind its own eight bytes.
		start := s.offset - d.moofLen - 8
		if start < 0 || start+s.size > len(mdat) {
			return fmt.Errorf("%w: a sample at %d of %d bytes in a data box of %d", ErrContainer, start, s.size, len(mdat))
		}
		if d.onSample != nil {
			d.onSample(Sample{
				Key:  s.flags&sampleNotSync == 0,
				PTS:  time.Duration(pts(s)) * time.Second / scale,
				Wall: now.Add(-time.Duration(last-pts(s)) * time.Second / scale),
				Data: append([]byte(nil), mdat[start:start+s.size]...),
			})
		}
	}
	return nil
}
