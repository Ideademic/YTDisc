package main

// Pure-Go MP4 stream-copy muxer. Replaces our previous use of
// `ffmpeg -c copy -i video -i audio out.mp4` so the app no longer
// needs ffmpeg installed for downloads — yt-dlp's separate
// video-only and audio-only streams are merged here instead.
//
// Inputs are the per-stream MP4s yt-dlp leaves on disk when its
// own merger is unavailable: `Title.f137.mp4` (H.264 in mp4) and
// `Title.f140.m4a` (AAC in mp4). Output is a regular multi-track
// MP4 with both samples interleaved at the file level
// ([ftyp][mdat][moov]). No transcoding.
//
// We use mp4ff's box-tree representation to read each input's moov
// and rewrite the chunk-offset tables (stco / co64) so they point at
// the new positions in the merged file. Sample data is stream-copied
// from each input file directly to the output without going through
// memory, so the muxer scales to any size yt-dlp can produce.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Eyevinn/mp4ff/mp4"
)

// muxAVtoMP4 combines a video-only MP4 and an audio-only MP4 (or
// .m4a — same container) into one multi-track MP4 at outPath. Both
// inputs are expected to contain exactly one track each. The
// resulting file is byte-identical in sample data to what
// `ffmpeg -c copy -i video -i audio out.mp4` would produce, modulo
// box ordering (we use mdat-then-moov, like ffmpeg's default).
func muxAVtoMP4(videoPath, audioPath, outPath string) error {
	videoFile, err := os.Open(videoPath)
	if err != nil {
		return fmt.Errorf("open video: %w", err)
	}
	defer videoFile.Close()

	audioFile, err := os.Open(audioPath)
	if err != nil {
		return fmt.Errorf("open audio: %w", err)
	}
	defer audioFile.Close()

	// DecModeLazyMdat skips reading the actual sample bytes — we
	// only want the box tree. We'll stream the mdat payload
	// directly from the source files later (no full-file load).
	video, err := mp4.DecodeFile(videoFile, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		return fmt.Errorf("parse video: %w", err)
	}
	audio, err := mp4.DecodeFile(audioFile, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		return fmt.Errorf("parse audio: %w", err)
	}

	if err := validateSingleTrackMP4(video, "video"); err != nil {
		return err
	}
	if err := validateSingleTrackMP4(audio, "audio"); err != nil {
		return err
	}

	videoTrak := video.Moov.Traks[0]
	audioTrak := audio.Moov.Traks[0]

	// Compute the byte ranges of each input's mdat payload. The
	// payload (sample data) starts after the mdat box header (8 or
	// 16 bytes) and runs to the end of the box.
	videoMdatStart := video.Mdat.PayloadAbsoluteOffset()
	videoMdatSize := video.Mdat.Size() - video.Mdat.HeaderSize()
	audioMdatStart := audio.Mdat.PayloadAbsoluteOffset()
	audioMdatSize := audio.Mdat.Size() - audio.Mdat.HeaderSize()

	// Output layout: [ftyp][mdat header][video samples][audio samples][moov]
	//
	// Putting moov last lets us know all sample offsets up-front
	// without solving the chicken-and-egg of moov-first (where
	// stco offsets depend on moov size, which depends on stco
	// size — itself fixed, but the mvex/udta children aren't).
	ftyp := video.Ftyp
	if ftyp == nil {
		return errors.New("video: missing ftyp")
	}
	ftypSize := ftyp.Size()

	mdatPayload := videoMdatSize + audioMdatSize
	// 8-byte mdat header is enough for payloads < 4 GB. yt-dlp's
	// per-stream files are well under that, but use the 16-byte
	// "largesize" form if a future huge file ever needs it.
	mdatHdrSize := uint64(8)
	if mdatPayload+mdatHdrSize > 0xFFFFFFFF {
		mdatHdrSize = 16
	}
	mdatTotalSize := mdatHdrSize + mdatPayload

	// Where each track's sample data lives in the OUTPUT file.
	videoNewBase := ftypSize + mdatHdrSize
	audioNewBase := videoNewBase + videoMdatSize

	// Rewrite stco/co64 in each track. Original entries point into
	// each source file's mdat payload; new entries must point into
	// the merged file's mdat at the corresponding byte position.
	if err := shiftChunkOffsets(videoTrak, videoMdatStart, videoNewBase); err != nil {
		return fmt.Errorf("video stco: %w", err)
	}
	if err := shiftChunkOffsets(audioTrak, audioMdatStart, audioNewBase); err != nil {
		return fmt.Errorf("audio stco: %w", err)
	}

	// Build the output moov. Reuse the video's moov as the base
	// (it has the right Mvhd) and replace its track list with both
	// tracks from the inputs.
	newMoov := mp4.NewMoovBox()
	if video.Moov.Mvhd != nil {
		newMoov.AddChild(video.Moov.Mvhd)
	}

	// Make sure track IDs are unique. If audio reuses the video's
	// track ID (both inputs typically come out of yt-dlp with
	// TrackID=1), bump audio so the muxed file has 1 + 2.
	if videoTrak.Tkhd != nil && audioTrak.Tkhd != nil {
		if audioTrak.Tkhd.TrackID == videoTrak.Tkhd.TrackID {
			audioTrak.Tkhd.TrackID = videoTrak.Tkhd.TrackID + 1
		}
	}
	newMoov.AddChild(videoTrak)
	newMoov.AddChild(audioTrak)

	// Mvhd.NextTrackID must exceed every existing track ID, and
	// Mvhd.Duration must be at least the longest track's duration —
	// the source video's Mvhd.Duration only reflects the video
	// track, but audio is often slightly longer (different sample
	// boundaries) and the spec wants `max(track durations)`. Most
	// browsers ignore Mvhd.Duration and use Tkhd.Duration directly,
	// but ffprobe / QuickTime / seekbars in some apps will display
	// the wrong number if we leave it stale.
	if newMoov.Mvhd != nil {
		var maxTrackID uint32
		var maxDuration uint64
		for _, t := range newMoov.Traks {
			if t.Tkhd == nil {
				continue
			}
			if t.Tkhd.TrackID > maxTrackID {
				maxTrackID = t.Tkhd.TrackID
			}
			if t.Tkhd.Duration > maxDuration {
				maxDuration = t.Tkhd.Duration
			}
		}
		newMoov.Mvhd.NextTrackID = maxTrackID + 1
		if maxDuration > 0 {
			newMoov.Mvhd.Duration = maxDuration
		}
	}

	// Now write the output file.
	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	// Defer close + return any close error if the encode succeeded.
	closed := false
	defer func() {
		if !closed {
			_ = out.Close()
		}
	}()

	if err := ftyp.Encode(out); err != nil {
		return fmt.Errorf("write ftyp: %w", err)
	}
	if err := writeMdatHeader(out, mdatTotalSize, mdatHdrSize == 16); err != nil {
		return fmt.Errorf("write mdat header: %w", err)
	}
	// Stream-copy the sample payloads from each input directly to
	// the output. No allocation of the full mdat in memory.
	if _, err := videoFile.Seek(int64(videoMdatStart), io.SeekStart); err != nil {
		return fmt.Errorf("seek video mdat: %w", err)
	}
	if _, err := io.CopyN(out, videoFile, int64(videoMdatSize)); err != nil {
		return fmt.Errorf("copy video mdat: %w", err)
	}
	if _, err := audioFile.Seek(int64(audioMdatStart), io.SeekStart); err != nil {
		return fmt.Errorf("seek audio mdat: %w", err)
	}
	if _, err := io.CopyN(out, audioFile, int64(audioMdatSize)); err != nil {
		return fmt.Errorf("copy audio mdat: %w", err)
	}
	if err := newMoov.Encode(out); err != nil {
		return fmt.Errorf("write moov: %w", err)
	}

	closed = true
	if err := out.Close(); err != nil {
		return fmt.Errorf("close output: %w", err)
	}
	return nil
}

// validateSingleTrackMP4 enforces the inputs we expect from yt-dlp:
// a parseable, non-fragmented MP4 with exactly one track and a
// non-empty mdat.
//
// We reject fragmented MP4s (those with an mvex box in moov)
// because fragmented samples live in moof boxes, not in stco/co64.
// Our chunk-offset rewriter would silently no-op on the wrong
// tables and produce a broken output. yt-dlp's per-stream YouTube
// downloads aren't fragmented today, but rejecting the case
// explicitly means a future format change becomes a clean error
// instead of silent corruption.
func validateSingleTrackMP4(f *mp4.File, label string) error {
	if f.Moov == nil {
		return fmt.Errorf("%s: no moov box", label)
	}
	if f.Mdat == nil {
		return fmt.Errorf("%s: no mdat box", label)
	}
	if f.Moov.Mvex != nil {
		return fmt.Errorf("%s: fragmented MP4 (mvex present) — muxer only supports non-fragmented inputs", label)
	}
	if len(f.Moov.Traks) == 0 {
		return fmt.Errorf("%s: no tracks", label)
	}
	if len(f.Moov.Traks) > 1 {
		return fmt.Errorf("%s: expected 1 track, got %d", label, len(f.Moov.Traks))
	}
	trak := f.Moov.Traks[0]
	if trak.Mdia == nil || trak.Mdia.Minf == nil || trak.Mdia.Minf.Stbl == nil {
		return fmt.Errorf("%s: track missing mdia/minf/stbl", label)
	}
	stbl := trak.Mdia.Minf.Stbl
	if stbl.Stco == nil && stbl.Co64 == nil {
		return fmt.Errorf("%s: track has neither stco nor co64", label)
	}
	return nil
}

// shiftChunkOffsets walks a track's chunk-offset table (stco for
// 32-bit offsets, co64 for 64-bit) and rewrites every entry from
// (offset_in_source = oldBase + delta) to (offset_in_output =
// newBase + delta). yt-dlp output has one of stco or co64 per
// track, never both; we handle whichever is present.
//
// stco is 32-bit. If newBase + any old delta overflows uint32, we
// can't keep stco — we'd need to upgrade the box to co64. That's
// not implemented because no realistic yt-dlp single-stream MP4
// pushes a track past 4 GB; we return an error in that case so
// the caller can surface it rather than silently corrupting offsets.
func shiftChunkOffsets(trak *mp4.TrakBox, oldBase, newBase uint64) error {
	stbl := trak.Mdia.Minf.Stbl
	if stbl.Stco != nil {
		for i, off := range stbl.Stco.ChunkOffset {
			delta := uint64(off) - oldBase
			abs := newBase + delta
			if abs > 0xFFFFFFFF {
				return errors.New("output offset exceeds 4 GB; stco→co64 upgrade not implemented")
			}
			stbl.Stco.ChunkOffset[i] = uint32(abs)
		}
	}
	if stbl.Co64 != nil {
		for i, off := range stbl.Co64.ChunkOffset {
			delta := off - oldBase
			stbl.Co64.ChunkOffset[i] = newBase + delta
		}
	}
	return nil
}

// writeMdatHeader writes the box header for an mdat with the given
// total size (header + payload). For sizes ≤ 4 GB - 1 the standard
// 8-byte header is used; for larger, the "largesize" form with a
// 64-bit size field is used (size32=1, then 64-bit actual size).
func writeMdatHeader(w io.Writer, totalSize uint64, large bool) error {
	if large {
		var hdr [16]byte
		binary.BigEndian.PutUint32(hdr[0:4], 1) // 1 = "see largesize"
		copy(hdr[4:8], []byte("mdat"))
		binary.BigEndian.PutUint64(hdr[8:16], totalSize)
		_, err := w.Write(hdr[:])
		return err
	}
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(totalSize))
	copy(hdr[4:8], []byte("mdat"))
	_, err := w.Write(hdr[:])
	return err
}
