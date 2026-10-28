package tracker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxReportContextURLBytes   = 2048
	MaxReportContextFieldBytes = 256
)

var ErrInvalidReportContext = errors.New("report_context accepts only url (at most 2048 bytes) and app_build, browser, os, viewport and captured_at (at most 256 bytes each) as single-line strings")

type ReportContext struct {
	URL        string `json:"url,omitempty"`
	AppBuild   string `json:"app_build,omitempty"`
	Browser    string `json:"browser,omitempty"`
	OS         string `json:"os,omitempty"`
	Viewport   string `json:"viewport,omitempty"`
	CapturedAt string `json:"captured_at,omitempty"`
}

func DecodeReportContext(raw json.RawMessage) (*ReportContext, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value ReportContext
	if err := decoder.Decode(&value); err != nil {
		return nil, ErrInvalidReportContext
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidReportContext
	}
	if err := value.Validate(); err != nil {
		return nil, err
	}
	return value.Normalized(), nil
}

func (r ReportContext) Validate() error {
	for _, field := range []struct {
		value string
		limit int
	}{
		{r.URL, MaxReportContextURLBytes},
		{r.AppBuild, MaxReportContextFieldBytes},
		{r.Browser, MaxReportContextFieldBytes},
		{r.OS, MaxReportContextFieldBytes},
		{r.Viewport, MaxReportContextFieldBytes},
		{r.CapturedAt, MaxReportContextFieldBytes},
	} {
		if len(field.value) > field.limit || !utf8.ValidString(field.value) || strings.ContainsFunc(field.value, invalidReportContextRune) {
			return ErrInvalidReportContext
		}
	}
	return nil
}

func invalidReportContextRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

func (r ReportContext) Normalized() *ReportContext {
	normalized := ReportContext{
		URL:        strings.TrimSpace(r.URL),
		AppBuild:   strings.TrimSpace(r.AppBuild),
		Browser:    strings.TrimSpace(r.Browser),
		OS:         strings.TrimSpace(r.OS),
		Viewport:   strings.TrimSpace(r.Viewport),
		CapturedAt: strings.TrimSpace(r.CapturedAt),
	}
	if normalized == (ReportContext{}) {
		return nil
	}
	return &normalized
}

func (r *ReportContext) DescribesEnvironment() bool {
	return r != nil && strings.TrimSpace(r.Browser) != "" && strings.TrimSpace(r.AppBuild) != ""
}
