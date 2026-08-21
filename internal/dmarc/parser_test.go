package dmarc

import (
	"strings"
	"testing"
)

func TestParseAggregateReport(t *testing.T) {
	raw := `From: reports@provider.example
Content-Type: multipart/mixed; boundary=report

--report
Content-Type: text/plain

Attached.
--report
Content-Type: application/dmarc+xml; name="report.xml"
Content-Disposition: attachment; filename="report.xml"

<?xml version="1.0"?>
<feedback>
  <report_metadata>
    <org_name>Provider</org_name>
    <email>reports@provider.example</email>
    <report_id>abc-123</report_id>
    <date_range><begin>1700000000</begin><end>1700086400</end></date_range>
  </report_metadata>
  <policy_published><domain>solarpass.one</domain><adkim>s</adkim><aspf>r</aspf><p>quarantine</p><sp>none</sp><pct>75</pct></policy_published>
  <record>
    <row><source_ip>192.0.2.1</source_ip><count>4</count><policy_evaluated><disposition>none</disposition><dkim>pass</dkim><spf>fail</spf></policy_evaluated></row>
    <identifiers><header_from>solarpass.one</header_from><envelope_from>solarpass.one</envelope_from><envelope_to>receiver.example</envelope_to></identifiers>
  </record>
</feedback>
--report--
`
	reports, err := Parse([]byte(strings.ReplaceAll(raw, `\r\n`, "\r\n")))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(reports))
	}
	report := reports[0]
	if report.ReporterOrg != "Provider" || report.ReportID != "abc-123" || report.Domain != "solarpass.one" {
		t.Fatalf("report metadata = %#v", report)
	}
	if report.Percentage != 75 || len(report.Records) != 1 || report.Records[0].SPF != "fail" {
		t.Fatalf("report policy/records = %#v", report)
	}
}

func TestParseRejectsMissingReport(t *testing.T) {
	_, err := Parse([]byte("From: sender@example.test\r\nContent-Type: text/plain\r\n\r\nhello\r\n"))
	if err == nil || !strings.Contains(err.Error(), "no DMARC report") {
		t.Fatalf("Parse() error = %v", err)
	}
}
