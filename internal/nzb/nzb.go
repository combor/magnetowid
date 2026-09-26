// Package nzb wraps a vodarr job reference in an NZB document, which
// Sonarr/Radarr pass unchanged from the indexer to the download client.
package nzb

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
)

const (
	namespace = "http://www.newzbin.com/DTD/2003/nzb"
	metaType  = "vodarr"
)

// ErrNotVodarr is returned by Decode for NZBs that carry no vodarr reference.
var ErrNotVodarr = errors.New("nzb: not a vodarr job")

// Ref identifies content on a provider.
type Ref struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Duration int    `json:"duration,omitempty"` // seconds
}

// XMLName is untagged so Encode can set the namespace and Decode accepts any.
type document struct {
	XMLName xml.Name
	Head    head   `xml:"head"`
	Files   []file `xml:"file"`
}

type head struct {
	Meta []meta `xml:"meta"`
}

type meta struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

type file struct {
	Poster   string    `xml:"poster,attr"`
	Date     int64     `xml:"date,attr"`
	Subject  string    `xml:"subject,attr"`
	Groups   []string  `xml:"groups>group"`
	Segments []segment `xml:"segments>segment"`
}

type segment struct {
	Bytes  int    `xml:"bytes,attr"`
	Number int    `xml:"number,attr"`
	ID     string `xml:",chardata"`
}

// Encode returns an NZB document carrying r.
func Encode(r Ref) ([]byte, error) {
	if r.Provider == "" || r.ID == "" {
		return nil, errors.New("nzb: provider and id are required")
	}
	value, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	doc := document{
		XMLName: xml.Name{Space: namespace, Local: "nzb"},
		Head:    head{Meta: []meta{{Type: metaType, Value: string(value)}}},
		Files: []file{{
			Poster:   "vodarr",
			Subject:  "vodarr placeholder",
			Groups:   []string{"alt.binaries.vodarr"},
			Segments: []segment{{Bytes: 0, Number: 1, ID: "placeholder@vodarr"}},
		}},
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), body...), nil
}

// Decode extracts the vodarr reference from an NZB document.
func Decode(b []byte) (Ref, error) {
	var doc document
	if err := xml.Unmarshal(b, &doc); err != nil {
		return Ref{}, fmt.Errorf("nzb: %w", err)
	}
	if doc.XMLName.Local != "nzb" {
		return Ref{}, fmt.Errorf("nzb: unexpected root element %q", doc.XMLName.Local)
	}
	for _, m := range doc.Head.Meta {
		if m.Type != metaType {
			continue
		}
		var r Ref
		if err := json.Unmarshal([]byte(m.Value), &r); err != nil {
			return Ref{}, fmt.Errorf("nzb: bad vodarr meta: %w", err)
		}
		if r.Provider == "" || r.ID == "" {
			return Ref{}, errors.New("nzb: vodarr meta lacks provider or id")
		}
		return r, nil
	}
	return Ref{}, ErrNotVodarr
}
