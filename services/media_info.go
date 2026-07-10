package services

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	ffprobe "gopkg.in/vansante/go-ffprobe.v2"
)

// VideoInfo holds extracted metadata for a video file. Modelled after the
// output of the standalone `mediainfo` CLI — we extract via ffprobe but
// present the same level of detail users expect from a mediainfo dump.
type VideoInfo struct {
	FileName string `json:"file_name"`

	// Container / General
	Format        string  `json:"format"`                   // matroska, mp4, avi
	FormatVersion string  `json:"format_version,omitempty"` // "Version 2" for MKVv2
	Duration      float64 `json:"duration"`                 // seconds
	Size          int64   `json:"size"`                     // bytes
	Bitrate       int64   `json:"bitrate"`                  // overall bps
	WritingApp    string  `json:"writing_app,omitempty"`    // muxer/encoder app
	EncodedDate   string  `json:"encoded_date,omitempty"`

	// Video
	VideoCodec     string `json:"video_codec"`   // h264, hevc, av1, mpeg2video
	VideoProfile   string `json:"video_profile"` // High, Main@Main, Main 10, etc.
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	DisplayAspect  string `json:"display_aspect,omitempty"` // "16:9", "4:3"
	FrameRate      string `json:"frame_rate"`               // "23.976", "29.97"
	PixelFormat    string `json:"pixel_format"`             // yuv420p, yuv420p10le
	BitDepth       int    `json:"bit_depth"`                // 8, 10, 12
	ChromaSub      string `json:"chroma_sub,omitempty"`     // "4:2:0", "4:4:4"
	ScanType       string `json:"scan_type,omitempty"`      // "Progressive", "Interlaced"
	ColorSpace     string `json:"color_space,omitempty"`
	ColorTransfer  string `json:"color_transfer,omitempty"`  // smpte2084 = HDR10
	ColorPrimaries string `json:"color_primaries,omitempty"` // bt2020 = wide gamut
	HDR            string `json:"hdr,omitempty"`             // "HDR10", "Dolby Vision", "HLG", ""
	// HDR side-data details, when the source carries them. All are
	// optional — anime is typically SDR and even HDR remuxes don't
	// always carry the static (mastering display) or dynamic (DV)
	// metadata. Site renders these on the Media Info card when
	// present and shows nothing when absent.
	MaxCLL               int     `json:"max_cll,omitempty"`                 // nits, max single pixel
	MaxFALL              int     `json:"max_fall,omitempty"`                // nits, max frame average
	MasterDisplayMaxNits float64 `json:"master_display_max_nits,omitempty"` // mastering display peak
	MasterDisplayMinNits float64 `json:"master_display_min_nits,omitempty"` // mastering display floor
	// DolbyVisionProfile is the DoVi profile (5, 7, 8, 10) when
	// ffprobe reports a DOVI configuration record. 0 when absent.
	DolbyVisionProfile  int `json:"dolby_vision_profile,omitempty"`
	DolbyVisionLevel    int `json:"dolby_vision_level,omitempty"`
	DolbyVisionRPU      bool `json:"dolby_vision_rpu,omitempty"`
	DolbyVisionBL       bool `json:"dolby_vision_bl,omitempty"`
	DolbyVisionEL       bool `json:"dolby_vision_el,omitempty"`
	VideoBitrate   int64  `json:"video_bitrate"`

	// Audio tracks
	AudioTracks []AudioTrack `json:"audio_tracks"`

	// Subtitle tracks
	SubtitleTracks []SubTrack `json:"subtitle_tracks"`

	// Chapters
	Chapters []Chapter `json:"chapters,omitempty"`
}

// AudioTrack describes one audio stream.
type AudioTrack struct {
	Codec      string `json:"codec"`   // aac, flac, dts, truehd, eac3, ac3
	Profile    string `json:"profile"` // DTS-HD MA, LC, etc.
	Language   string `json:"language"`
	Channels   int    `json:"channels"` // 2, 6 (5.1), 8 (7.1)
	Layout     string `json:"layout"`   // stereo, 5.1, 7.1
	Bitrate    int64  `json:"bitrate"`
	SampleRate int    `json:"sample_rate"` // 44100, 48000
	Title      string `json:"title,omitempty"`
	Default    bool   `json:"default"`
}

// SubTrack describes one subtitle stream.
type SubTrack struct {
	Codec    string `json:"codec"` // ass, srt, subrip, hdmv_pgs, dvd_subtitle
	Language string `json:"language"`
	Title    string `json:"title"`
	Default  bool   `json:"default"`
	Forced   bool   `json:"forced"`
}

// Chapter describes a named chapter marker.
type Chapter struct {
	Start float64 `json:"start"` // seconds
	End   float64 `json:"end"`   // seconds
	Title string  `json:"title"`
}

// ProbeVideo extracts detailed metadata from a video file using ffprobe.
func ProbeVideo(ctx context.Context, filePath string) (*VideoInfo, error) {
	data, err := ffprobe.ProbeURL(ctx, filePath)
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}

	info := &VideoInfo{
		FileName: filepath.Base(filePath),
		Format:   data.Format.FormatName,
		Size:     parseInt64(data.Format.Size),
	}

	info.Duration = data.Format.DurationSeconds
	info.Bitrate = parseInt64(data.Format.BitRate)

	// Container-level tags (encoder, creation date).
	if enc, err := data.Format.TagList.GetString("encoder"); err == nil {
		info.WritingApp = enc
	}
	if date, err := data.Format.TagList.GetString("creation_time"); err == nil {
		info.EncodedDate = date
	}

	// Process streams.
	for _, s := range data.Streams {
		switch s.CodecType {
		case "video":
			if info.VideoCodec != "" {
				continue // take first video stream only
			}
			info.VideoCodec = s.CodecName
			info.VideoProfile = s.Profile
			info.Width = s.Width
			info.Height = s.Height
			info.PixelFormat = s.PixFmt
			info.VideoBitrate = parseInt64(s.BitRate)
			info.DisplayAspect = s.DisplayAspectRatio

			// Frame rate from r_frame_rate (e.g. "24000/1001").
			if s.RFrameRate != "" {
				info.FrameRate = simplifyFrameRate(s.RFrameRate)
			}

			// Bit depth from pix_fmt.
			info.BitDepth = bitDepthFromPixFmt(s.PixFmt)
			if bps := s.BitsPerRawSample; bps != "" {
				if v, err := strconv.Atoi(bps); err == nil && v > 0 {
					info.BitDepth = v
				}
			}

			// Chroma subsampling from pixel format.
			info.ChromaSub = chromaSubFromPixFmt(s.PixFmt)

			// Scan type from field_order.
			switch strings.ToLower(s.FieldOrder) {
			case "progressive", "":
				info.ScanType = "Progressive"
			case "tt", "bb":
				info.ScanType = "Interlaced"
			case "tb", "bt":
				info.ScanType = "MBAFF"
			default:
				if s.FieldOrder != "" {
					info.ScanType = s.FieldOrder
				}
			}

			// HDR detection.
			info.ColorSpace = s.ColorSpace
			info.ColorTransfer = s.ColorTransfer
			info.ColorPrimaries = s.ColorPrimaries
			info.HDR = detectHDR(s)
			populateHDRSideData(info, s)

		case "audio":
			lang := s.Tags.Language
			if lang == "" {
				lang = "und"
			}
			at := AudioTrack{
				Codec:      s.CodecName,
				Profile:    s.Profile,
				Language:   lang,
				Channels:   s.Channels,
				Layout:     s.ChannelLayout,
				Bitrate:    parseInt64(s.BitRate),
				SampleRate: parseInt(s.SampleRate),
				Title:      s.Tags.Title,
				Default:    s.Disposition.Default == 1,
			}
			info.AudioTracks = append(info.AudioTracks, at)

		case "subtitle":
			lang := s.Tags.Language
			if lang == "" {
				lang = "und"
			}
			st := SubTrack{
				Codec:    s.CodecName,
				Language: lang,
				Title:    s.Tags.Title,
				Default:  s.Disposition.Default == 1,
				Forced:   s.Disposition.Forced == 1,
			}
			info.SubtitleTracks = append(info.SubtitleTracks, st)
		}
	}

	// Chapters.
	for _, ch := range data.Chapters {
		info.Chapters = append(info.Chapters, Chapter{
			Start: ch.StartTime().Seconds(),
			End:   ch.EndTime().Seconds(),
			Title: ch.Title(),
		})
	}

	return info, nil
}

// DurationStr returns a human-readable duration like "1h 24m 30s".
func (v *VideoInfo) DurationStr() string {
	d := int(v.Duration)
	h := d / 3600
	m := (d % 3600) / 60
	s := d % 60
	if h > 0 {
		return fmt.Sprintf("%dh %02dm %02ds", h, m, s)
	}
	return fmt.Sprintf("%dm %02ds", m, s)
}

// ResolutionLabel returns "4K", "1080p", "720p", etc.
func (v *VideoInfo) ResolutionLabel() string {
	switch {
	case v.Height >= 2160:
		return "4K"
	case v.Height >= 1080:
		return "1080p"
	case v.Height >= 720:
		return "720p"
	case v.Height >= 576:
		return "576p"
	case v.Height >= 480:
		return "480p"
	default:
		return fmt.Sprintf("%dp", v.Height)
	}
}

// populateHDRSideData walks ffprobe's side_data_list and fills the
// HDR detail fields on info. ffprobe wraps the static HDR10 metadata
// (mastering display, content light level) into typed accessors;
// Dolby Vision is reported as an "unknown" side data type (DOVI
// configuration record) so we sniff the raw field map.
//
// Every value is optional — missing side data leaves the field at its
// zero value and the site template hides the corresponding row.
func populateHDRSideData(info *VideoInfo, s *ffprobe.Stream) {
	if md, err := s.SideDataList.GetMasteringDisplayMetadata(); err == nil && md != nil {
		// Mastering display luminance reports in cd/m² already.
		// FFmpeg's older builds emit it as 0.0001-scale fixed point;
		// the FlexFloat in the vendor library normalises that for us.
		info.MasterDisplayMaxNits = float64(md.MaxLuminance)
		info.MasterDisplayMinNits = float64(md.MinLuminance)
	}
	if cll, err := s.SideDataList.GetContentLightLevel(); err == nil && cll != nil {
		info.MaxCLL = cll.MaxContent
		info.MaxFALL = cll.MaxAverage
	}
	// Dolby Vision sits in a side data record ffprobe doesn't have a
	// typed accessor for. Iterate to find one whose type names DOVI.
	for _, sd := range s.SideDataList {
		t := strings.ToLower(sd.Type)
		if !strings.Contains(t, "dovi") && !strings.Contains(t, "dolby vision") {
			continue
		}
		uk, ok := sd.Data.(*ffprobe.SideDataUnknown)
		if !ok || uk == nil {
			continue
		}
		// SideDataUnknown is a Tags alias — GetInt/GetString on the
		// keys ffprobe surfaces ("dv_profile", "dv_level", "rpu_present_flag",
		// "bl_present_flag", "el_present_flag"). Missing keys leave
		// the corresponding info field at zero.
		tagsAlias := ffprobe.Tags(*uk)
		if v, err := tagsAlias.GetInt("dv_profile"); err == nil {
			info.DolbyVisionProfile = int(v)
		}
		if v, err := tagsAlias.GetInt("dv_level"); err == nil {
			info.DolbyVisionLevel = int(v)
		}
		if v, err := tagsAlias.GetInt("rpu_present_flag"); err == nil {
			info.DolbyVisionRPU = v != 0
		}
		if v, err := tagsAlias.GetInt("bl_present_flag"); err == nil {
			info.DolbyVisionBL = v != 0
		}
		if v, err := tagsAlias.GetInt("el_present_flag"); err == nil {
			info.DolbyVisionEL = v != 0
		}
		break
	}
}

func detectHDR(s *ffprobe.Stream) string {
	ct := strings.ToLower(s.ColorTransfer)
	cp := strings.ToLower(s.ColorPrimaries)

	// Check side data for Dolby Vision.
	for _, sd := range s.SideDataList {
		if strings.Contains(strings.ToLower(sd.Type), "dolby vision") {
			return "Dolby Vision"
		}
	}
	if ct == "smpte2084" && cp == "bt2020" {
		return "HDR10"
	}
	if ct == "arib-std-b67" {
		return "HLG"
	}
	if cp == "bt2020" {
		return "HDR"
	}
	return ""
}

func bitDepthFromPixFmt(pf string) int {
	if strings.Contains(pf, "10le") || strings.Contains(pf, "10be") || strings.Contains(pf, "p10") {
		return 10
	}
	if strings.Contains(pf, "12le") || strings.Contains(pf, "12be") || strings.Contains(pf, "p12") {
		return 12
	}
	return 8
}

func simplifyFrameRate(rfr string) string {
	parts := strings.Split(rfr, "/")
	if len(parts) != 2 {
		return rfr
	}
	num, err1 := strconv.ParseFloat(parts[0], 64)
	den, err2 := strconv.ParseFloat(parts[1], 64)
	if err1 != nil || err2 != nil || den == 0 {
		return rfr
	}
	fps := num / den
	// Round to 3 decimals.
	fps = math.Round(fps*1000) / 1000
	return strconv.FormatFloat(fps, 'f', -1, 64)
}

func parseInt64(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

func parseInt(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}

// chromaSubFromPixFmt derives the chroma subsampling label from ffprobe's
// pix_fmt string (e.g. "yuv420p" → "4:2:0", "yuv444p10le" → "4:4:4").
func chromaSubFromPixFmt(pf string) string {
	pf = strings.ToLower(pf)
	switch {
	case strings.Contains(pf, "444"):
		return "4:4:4"
	case strings.Contains(pf, "422"):
		return "4:2:2"
	case strings.Contains(pf, "420"):
		return "4:2:0"
	case strings.Contains(pf, "411"):
		return "4:1:1"
	case strings.Contains(pf, "410"):
		return "4:1:0"
	}
	return ""
}
