package rtc_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hyz-is/arandu-rtc"
)

func TestDemuxerReadsTheTrackAndEverySample(t *testing.T) {
	data := clip(t, "h264.mp4")
	// One piece, and one byte at a time: a pipe splits the stream anywhere.
	for _, size := range []int{len(data), 4096, 7, 1} {
		p := demux(t, data, size)
		if len(p.tracks) != 1 {
			t.Fatalf("size %d: %d tracks, want 1", size, len(p.tracks))
		}
		tr := p.tracks[0]
		if !strings.HasPrefix(tr.Codec, "avc1.") || len(tr.Codec) != len("avc1.640028") {
			t.Errorf("size %d: codec %q is not an avc1 string", size, tr.Codec)
		}
		if tr.Width != 160 || tr.Height != 120 {
			t.Errorf("size %d: picture %dx%d, want 160x120", size, tr.Width, tr.Height)
		}
		if len(tr.Description) < 7 || tr.Description[0] != 1 {
			t.Errorf("size %d: description %x is not an avcC record", size, tr.Description)
		}
		if tr.Timescale == 0 {
			t.Errorf("size %d: no timescale", size)
		}
		if len(p.samples) != 30 {
			t.Fatalf("size %d: %d samples, want 30", size, len(p.samples))
		}
		for i, s := range p.samples {
			if want := i%10 == 0; s.Key != want {
				t.Errorf("size %d: sample %d key = %v, want %v", size, i, s.Key, want)
			}
			if want := time.Duration(i) * 100 * time.Millisecond; s.PTS != want {
				t.Errorf("size %d: sample %d at %s, want %s", size, i, s.PTS, want)
			}
			if !nalsOK(s.Data) {
				t.Errorf("size %d: sample %d is not length-prefixed NAL units", size, i)
			}
		}
	}
}

func TestDemuxerKeepsDecodeOrderWithReorderedPictures(t *testing.T) {
	p := demux(t, clip(t, "h264-bframes.mp4"), 1000)
	if len(p.samples) != 30 || len(p.tracks) != 1 {
		t.Fatalf("%d samples and %d tracks, want 30 and 1", len(p.samples), len(p.tracks))
	}
	if !p.samples[0].Key {
		t.Fatal("the first sample is not a keyframe")
	}
	reordered := false
	var maxPTS time.Duration
	for i, s := range p.samples {
		if i > 0 && s.PTS < p.samples[i-1].PTS {
			reordered = true
		}
		maxPTS = max(maxPTS, s.PTS)
		if !nalsOK(s.Data) {
			t.Fatalf("sample %d is not length-prefixed NAL units", i)
		}
	}
	if !reordered {
		t.Error("the fixture has B-frames, so a presentation time must go back somewhere: the composition offsets were not read")
	}
	if maxPTS < 2800*time.Millisecond {
		t.Errorf("the last presentation time is %s, want about 2.9s", maxPTS)
	}
}

func TestDemuxerReadsHEVC(t *testing.T) {
	p := demux(t, clip(t, "h265.mp4"), 333)
	if len(p.tracks) != 1 || len(p.samples) != 20 {
		t.Fatalf("%d tracks and %d samples, want 1 and 20", len(p.tracks), len(p.samples))
	}
	tr := p.tracks[0]
	if !strings.HasPrefix(tr.Codec, "hvc1.1.6.L") {
		t.Errorf("codec %q, want hvc1.1.6.L<level>... for Main profile", tr.Codec)
	}
	if len(tr.Description) < 23 || tr.Description[0] != 1 {
		t.Errorf("description %x is not an hvcC record", tr.Description)
	}
}

func TestDemuxerRefusesWhatIsNotFragmentedMP4(t *testing.T) {
	cases := map[string][]byte{
		"noise":              []byte("this is not an mp4 file at all, it is a sentence"),
		"a box to the end":   {0, 0, 0, 0, 'm', 'o', 'o', 'v'},
		"a box too big":      {0x7f, 0xff, 0xff, 0xff, 'm', 'd', 'a', 't'},
		"a fragment first":   append([]byte{0, 0, 0, 8}, "moof"...),
		"a header with none": append([]byte{0, 0, 0, 8}, "moov"...),
	}
	for name, data := range cases {
		d := rtc.NewDemuxer(nil, nil)
		_, err := d.Write(data)
		if !errors.Is(err, rtc.ErrContainer) {
			t.Errorf("%s: error %v, want ErrContainer", name, err)
		}
	}
}

func TestDemuxerNeverPanicsOnDamage(t *testing.T) {
	data := clip(t, "h264.mp4")
	for cut := 0; cut < len(data); cut += 997 {
		damaged := append([]byte(nil), data...)
		damaged[cut] ^= 0xff
		d := rtc.NewDemuxer(func(rtc.Track) {}, func(rtc.Sample) {})
		_, _ = d.Write(damaged)
	}
}
