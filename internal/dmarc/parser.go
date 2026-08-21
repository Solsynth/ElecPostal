package dmarc

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxReportBytes  int64 = 10 << 20
	maxArchiveFiles       = 32
)

type Report struct {
	AttachmentName  string
	ReporterOrg     string
	ReporterEmail   string
	ReportID        string
	DateBegin       *time.Time
	DateEnd         *time.Time
	Domain          string
	ADKIM           string
	ASPF            string
	Policy          string
	SubdomainPolicy string
	Percentage      int
	Records         []Record
}

type Record struct {
	SourceIP     string
	Count        int64
	Disposition  string
	DKIM         string
	SPF          string
	HeaderFrom   string
	EnvelopeFrom string
	EnvelopeTo   string
}

// Parse extracts DMARC aggregate reports from an RFC 5322 message. Reports
// may be attached as XML, gzip-compressed XML, or ZIP archives containing XML.
func Parse(raw []byte) ([]Report, error) {
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("read message: %w", err)
	}
	reports, err := parseEntity(message.Header, message.Body)
	if err != nil {
		return nil, err
	}
	if len(reports) == 0 {
		return nil, errors.New("no DMARC report attachment found")
	}
	return reports, nil
}

// ParseAttachment parses one decoded DMARC report attachment without requiring
// an enclosing RFC 5322 message.
func ParseAttachment(name string, reader io.Reader) ([]Report, error) {
	if reader == nil {
		return nil, errors.New("DMARC attachment reader is required")
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxReportBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read report attachment: %w", err)
	}
	if int64(len(data)) > maxReportBytes {
		return nil, fmt.Errorf("report attachment exceeds %d bytes", maxReportBytes)
	}
	return parseAttachment(data, filepath.Base(name))
}

func parseEntity(header mail.Header, body io.Reader) ([]Report, error) {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		mediaType = "text/plain"
	}
	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return nil, errors.New("multipart message has no boundary")
		}
		reader := multipart.NewReader(body, boundary)
		var reports []Report
		for {
			part, nextErr := reader.NextPart()
			if errors.Is(nextErr, io.EOF) {
				break
			}
			if nextErr != nil {
				return nil, fmt.Errorf("read MIME part: %w", nextErr)
			}
			partReports, parseErr := parseEntity(mail.Header(part.Header), part)
			_ = part.Close()
			if parseErr != nil {
				// Non-report MIME parts are expected. Preserve a malformed
				// candidate error only when the part looks like a report.
				if looksLikeReport(mail.Header(part.Header)) {
					return nil, parseErr
				}
				continue
			}
			reports = append(reports, partReports...)
		}
		return reports, nil
	}
	filename := attachmentFilename(header, params)
	if !looksLikeReportWithName(mediaType, filename) {
		return nil, nil
	}
	data, err := readDecodedBody(header, body)
	if err != nil {
		return nil, err
	}
	return parseAttachment(data, filename)
}

func readDecodedBody(header mail.Header, body io.Reader) ([]byte, error) {
	reader := body
	switch strings.ToLower(strings.TrimSpace(header.Get("Content-Transfer-Encoding"))) {
	case "base64":
		reader = base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		reader = quotedprintable.NewReader(body)
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxReportBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read report attachment: %w", err)
	}
	if int64(len(data)) > maxReportBytes {
		return nil, fmt.Errorf("report attachment exceeds %d bytes", maxReportBytes)
	}
	return data, nil
}

func attachmentFilename(header mail.Header, params map[string]string) string {
	_, dispositionParams, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
	if name := dispositionParams["filename"]; name != "" {
		return filepath.Base(name)
	}
	if name := params["name"]; name != "" {
		return filepath.Base(name)
	}
	return ""
}

func looksLikeReport(header mail.Header) bool {
	mediaType, params, _ := mime.ParseMediaType(header.Get("Content-Type"))
	return looksLikeReportWithName(mediaType, attachmentFilename(header, params))
}

func looksLikeReportWithName(mediaType, filename string) bool {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	filename = strings.ToLower(strings.TrimSpace(filename))
	return mediaType == "application/dmarc+xml" || strings.HasSuffix(filename, ".xml") || strings.HasSuffix(filename, ".xml.gz") || strings.HasSuffix(filename, ".zip")
}

func parseAttachment(data []byte, filename string) ([]Report, error) {
	lower := strings.ToLower(filename)
	if strings.HasSuffix(lower, ".zip") {
		return parseZip(data)
	}
	if strings.HasSuffix(lower, ".gz") {
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("open gzip report: %w", err)
		}
		defer reader.Close()
		decoded, err := io.ReadAll(io.LimitReader(reader, maxReportBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read gzip report: %w", err)
		}
		if int64(len(decoded)) > maxReportBytes {
			return nil, fmt.Errorf("decompressed report exceeds %d bytes", maxReportBytes)
		}
		data = decoded
	}
	var report xmlReport
	if err := xml.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("parse DMARC XML: %w", err)
	}
	return []Report{report.toReport(filename)}, nil
}

func parseZip(data []byte) ([]Report, error) {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open DMARC ZIP: %w", err)
	}
	if len(archive.File) > maxArchiveFiles {
		return nil, fmt.Errorf("DMARC ZIP contains more than %d files", maxArchiveFiles)
	}
	var reports []Report
	for _, file := range archive.File {
		if file.FileInfo().IsDir() || !strings.HasSuffix(strings.ToLower(file.Name), ".xml") {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open ZIP report %q: %w", file.Name, err)
		}
		content, readErr := io.ReadAll(io.LimitReader(reader, maxReportBytes+1))
		_ = reader.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read ZIP report %q: %w", file.Name, readErr)
		}
		if int64(len(content)) > maxReportBytes {
			return nil, fmt.Errorf("ZIP report %q exceeds %d bytes", file.Name, maxReportBytes)
		}
		var report xmlReport
		if err := xml.Unmarshal(content, &report); err != nil {
			return nil, fmt.Errorf("parse DMARC XML %q: %w", file.Name, err)
		}
		reports = append(reports, report.toReport(file.Name))
	}
	if len(reports) == 0 {
		return nil, errors.New("DMARC ZIP contains no XML reports")
	}
	return reports, nil
}

type xmlReport struct {
	XMLName  xml.Name    `xml:"feedback"`
	Metadata xmlMetadata `xml:"report_metadata"`
	Policy   xmlPolicy   `xml:"policy_published"`
	Records  []xmlRecord `xml:"record"`
}

type xmlMetadata struct {
	OrgName   string       `xml:"org_name"`
	Email     string       `xml:"email"`
	ReportID  string       `xml:"report_id"`
	DateRange xmlDateRange `xml:"date_range"`
}

type xmlDateRange struct {
	Begin int64 `xml:"begin"`
	End   int64 `xml:"end"`
}

type xmlPolicy struct {
	Domain          string `xml:"domain"`
	ADKIM           string `xml:"adkim"`
	ASPF            string `xml:"aspf"`
	Policy          string `xml:"p"`
	SubdomainPolicy string `xml:"sp"`
	Percentage      int    `xml:"pct"`
}

type xmlRecord struct {
	Row         xmlRow         `xml:"row"`
	Identifiers xmlIdentifiers `xml:"identifiers"`
}

type xmlRow struct {
	SourceIP        string             `xml:"source_ip"`
	Count           int64              `xml:"count"`
	PolicyEvaluated xmlPolicyEvaluated `xml:"policy_evaluated"`
}

type xmlPolicyEvaluated struct {
	Disposition string `xml:"disposition"`
	DKIM        string `xml:"dkim"`
	SPF         string `xml:"spf"`
}

type xmlIdentifiers struct {
	EnvelopeTo   string `xml:"envelope_to"`
	EnvelopeFrom string `xml:"envelope_from"`
	HeaderFrom   string `xml:"header_from"`
}

func (r xmlReport) toReport(filename string) Report {
	report := Report{AttachmentName: filename, ReporterOrg: r.Metadata.OrgName, ReporterEmail: r.Metadata.Email, ReportID: r.Metadata.ReportID, Domain: r.Policy.Domain, ADKIM: r.Policy.ADKIM, ASPF: r.Policy.ASPF, Policy: r.Policy.Policy, SubdomainPolicy: r.Policy.SubdomainPolicy, Percentage: r.Policy.Percentage}
	if r.Metadata.DateRange.Begin != 0 {
		value := time.Unix(r.Metadata.DateRange.Begin, 0).UTC()
		report.DateBegin = &value
	}
	if r.Metadata.DateRange.End != 0 {
		value := time.Unix(r.Metadata.DateRange.End, 0).UTC()
		report.DateEnd = &value
	}
	for _, record := range r.Records {
		report.Records = append(report.Records, Record{SourceIP: record.Row.SourceIP, Count: record.Row.Count, Disposition: record.Row.PolicyEvaluated.Disposition, DKIM: record.Row.PolicyEvaluated.DKIM, SPF: record.Row.PolicyEvaluated.SPF, HeaderFrom: record.Identifiers.HeaderFrom, EnvelopeFrom: record.Identifiers.EnvelopeFrom, EnvelopeTo: record.Identifiers.EnvelopeTo})
	}
	return report
}
