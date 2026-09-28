package nzb

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	want := Ref{Provider: "tvp", ID: "3336417", Duration: 542}
	b, err := Encode(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Decode(Encode(%+v)) = %+v", want, got)
	}
}

// Sonarr/Radarr require an "nzb" root in the newzbin namespace with a <file>.
func TestEncodePassesArrValidation(t *testing.T) {
	b, err := Encode(Ref{Provider: "tvp", ID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		XMLName xml.Name
		Files   []struct {
			XMLName xml.Name
		} `xml:"file"`
	}
	if err := xml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.XMLName.Local != "nzb" || doc.XMLName.Space != namespace {
		t.Fatalf("root = %+v", doc.XMLName)
	}
	if len(doc.Files) == 0 || doc.Files[0].XMLName.Space != namespace {
		t.Fatalf("files = %+v", doc.Files)
	}
}

func TestDecodeRejectsForeignNZB(t *testing.T) {
	foreign := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <head><meta type="name">Some.Release</meta></head>
  <file poster="x" date="1" subject="y"><groups><group>a.b</group></groups>
    <segments><segment bytes="1" number="1">id@x</segment></segments></file>
</nzb>`
	if _, err := Decode([]byte(foreign)); !errors.Is(err, ErrNotMagnetowid) {
		t.Fatalf("err = %v, want ErrNotMagnetowid", err)
	}
	if _, err := Decode([]byte("not xml")); err == nil {
		t.Fatal("expected error for garbage")
	}
	if _, err := Encode(Ref{Provider: "tvp"}); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("Encode without id: err = %v", err)
	}
}
