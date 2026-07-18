package gstreamer

import (
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"time"

	"server/settings"
)

const gstreamerSettingsKey = "gstreamer"

const minGSTVersion = 1.22

type Config struct {
	GSTVersion float64 `json:"GSTVersion"`
	GSTPath    string  `json:"GSTPath"`
	Source     string  `json:"Source"`
	MaxTasks   int     `json:"MaxTasks"`

	InactiveMinutes int `json:"InactiveMinutes"`

	AACBitrateKbps int `json:"AACBitrateKbps"`
	AACChannels    int `json:"AACChannels"`
	AACSamplerate  int `json:"AACSamplerate"`
	SegmentSeconds int `json:"SegmentSeconds"`
	AppSinkBuffers int `json:"appsinkBuffers"`

	TranscodeH264 bool `json:"TranscodeH264"`
	TranscodeH265 bool `json:"TranscodeH265"`
	TranscodeAV1  bool `json:"TranscodeAV1"`
	TranscodeVP9  bool `json:"TranscodeVP9"`
	VideoBitrate  int  `json:"VideoBitrate"`

	TempFS     bool `json:"tempfs"`
	TempFSRing int  `json:"tempfs_ring"`

	// Proxy (fork extension, not in upstream): GStreamer proxy mode. When on,
	// generated M3U playlists point supported (Matroska/WebM) entries at the
	// /gst/{hash}/master.m3u8 HLS transcode endpoint instead of the direct
	// /stream link, so any HLS-capable player gets the AAC-transcoded audio
	// without hand-building URLs. Other containers keep their direct links (the
	// pipeline only accepts mkv/webm). Replaces the former PlaylistHLS setting;
	// the old key is still read once for migration (see applySettingsConfig).
	Proxy bool `json:"Proxy"`

	// AudioLang (fork extension): preferred audio-track language for the HLS
	// transcode when the request doesn't pick a track explicitly (playlist
	// links carry no ?audio=). A canonical two-letter code ("ru", "en", …);
	// empty = current behavior, the container's first audio track. Matched
	// against the discoverer language tags via canonicalAudioLang, so "rus",
	// "ru-RU" and "Russian" in the container all match "ru".
	AudioLang string `json:"AudioLang"`
}

func DefaultConfig() Config {
	return applySettingsConfig(defaultConfigWithoutSettings()).normalized()
}

func defaultConfigWithoutSettings() Config {
	conf := Config{
		GSTVersion:      minGSTVersion,
		Source:          "stream",
		InactiveMinutes: 5,
		AACBitrateKbps:  256,
		SegmentSeconds:  6,
		AppSinkBuffers:  1000,
		VideoBitrate:    10_000,
		TempFS:          false,
	}

	if runtime.GOOS == "windows" {
		conf.GSTVersion = 1.28
		conf.GSTPath = `C:\Program Files\gstreamer\1.0\mingw_x86_64`
	}

	return conf
}

func (c Config) normalized() Config {
	if c.InactiveMinutes <= 0 {
		c.InactiveMinutes = 5
	}
	if c.AACBitrateKbps <= 0 {
		c.AACBitrateKbps = 256
	}
	if c.AACChannels < 0 {
		c.AACChannels = 0
	}
	if c.AACSamplerate < 0 {
		c.AACSamplerate = 0
	}
	if c.MaxTasks < 0 {
		c.MaxTasks = 0
	}
	if c.SegmentSeconds <= 0 {
		c.SegmentSeconds = 6
	}
	if c.AppSinkBuffers <= 0 {
		c.AppSinkBuffers = 1000
	}
	if c.VideoBitrate <= 0 {
		c.VideoBitrate = 10_000
	}
	if c.TempFSRing < 0 {
		c.TempFSRing = 0
	}
	if c.GSTVersion < minGSTVersion {
		c.GSTVersion = minGSTVersion
	}
	c.Source = strings.ToLower(strings.TrimSpace(c.Source))
	if c.Source != "play" {
		c.Source = "stream"
	}
	c.AudioLang = canonicalAudioLang(c.AudioLang)
	return c
}

func (c Config) inactiveDuration() time.Duration {
	return time.Duration(c.normalized().InactiveMinutes) * time.Minute
}

type storedConfig struct {
	GSTVersion *float64
	GSTPath    *string
	Source     *string
	MaxTasks   *int

	InactiveMinutes *int

	AACBitrateKbps *int
	AACChannels    *int
	AACSamplerate  *int
	SegmentSeconds *int
	AppSinkBuffers *int `json:"appsinkBuffers"`

	TranscodeH264 *bool
	TranscodeH265 *bool
	TranscodeAV1  *bool
	TranscodeVP9  *bool
	VideoBitrate  *int

	TempFS     *bool `json:"tempfs"`
	TempFSRing *int  `json:"tempfs_ring"`

	Proxy     *bool
	AudioLang *string

	// PlaylistHLS is the pre-rename key, kept for one-way migration into Proxy.
	PlaylistHLS *bool
}

func applySettingsConfig(conf Config) Config {
	if settings.Path == "" {
		return conf
	}

	db := settings.NewJsonDB()
	if db == nil {
		return conf
	}

	var data []byte
	for _, name := range []string{gstreamerSettingsKey, "GStreamer", "gst"} {
		data = db.Get("Settings", name)
		if len(data) > 0 {
			break
		}
	}
	if len(data) == 0 {
		return conf
	}

	var stored storedConfig
	if err := json.Unmarshal(data, &stored); err != nil {
		return conf
	}

	if stored.GSTVersion != nil {
		conf.GSTVersion = *stored.GSTVersion
	}
	if stored.GSTPath != nil {
		conf.GSTPath = *stored.GSTPath
	}
	if stored.Source != nil {
		conf.Source = *stored.Source
	}
	if stored.MaxTasks != nil {
		conf.MaxTasks = *stored.MaxTasks
	}
	if stored.InactiveMinutes != nil {
		conf.InactiveMinutes = *stored.InactiveMinutes
	}
	if stored.AACBitrateKbps != nil {
		conf.AACBitrateKbps = *stored.AACBitrateKbps
	}
	if stored.AACChannels != nil {
		conf.AACChannels = *stored.AACChannels
	}
	if stored.AACSamplerate != nil {
		conf.AACSamplerate = *stored.AACSamplerate
	}
	if stored.SegmentSeconds != nil {
		conf.SegmentSeconds = *stored.SegmentSeconds
	}
	if stored.AppSinkBuffers != nil {
		conf.AppSinkBuffers = *stored.AppSinkBuffers
	}
	if stored.TranscodeH264 != nil {
		conf.TranscodeH264 = *stored.TranscodeH264
	}
	if stored.TranscodeH265 != nil {
		conf.TranscodeH265 = *stored.TranscodeH265
	}
	if stored.TranscodeAV1 != nil {
		conf.TranscodeAV1 = *stored.TranscodeAV1
	}
	if stored.TranscodeVP9 != nil {
		conf.TranscodeVP9 = *stored.TranscodeVP9
	}
	if stored.VideoBitrate != nil {
		conf.VideoBitrate = *stored.VideoBitrate
	}
	if stored.TempFS != nil {
		conf.TempFS = *stored.TempFS
	}
	if stored.TempFSRing != nil {
		conf.TempFSRing = *stored.TempFSRing
	}
	// Migration: honor the old PlaylistHLS key first, then let a present Proxy
	// key win. Once the config is saved again it serializes only Proxy, so the
	// legacy key drops out of storage on the next SaveConfig.
	if stored.PlaylistHLS != nil {
		conf.Proxy = *stored.PlaylistHLS
	}
	if stored.Proxy != nil {
		conf.Proxy = *stored.Proxy
	}
	if stored.AudioLang != nil {
		conf.AudioLang = *stored.AudioLang
	}

	return conf
}

// ProxyMode reports whether GStreamer proxy mode is on: generated M3U
// playlists point supported (Matroska/WebM) entries at the HLS transcode
// endpoint. Reads the live config so a settings save takes effect on the next
// playlist fetch.
func ProxyMode() bool {
	return LoadConfig().Proxy
}

func LoadConfig() Config {
	return DefaultConfig()
}

func SaveConfig(conf Config) error {
	if settings.ReadOnly {
		return errors.New("read-only mode")
	}
	if settings.Path == "" {
		return errors.New("settings path is not configured")
	}

	conf = conf.normalized()
	db := settings.NewJsonDB()
	if db == nil {
		return errors.New("json db unavailable")
	}

	data, err := json.Marshal(conf)
	if err != nil {
		return err
	}

	db.Set("Settings", gstreamerSettingsKey, data)
	db.Rem("Settings", "gst")
	db.Rem("Settings", "GStreamer")
	return nil
}
