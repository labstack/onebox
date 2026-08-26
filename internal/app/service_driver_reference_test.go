package app

import (
	"reflect"
	"sort"
	"testing"
)

func TestServiceDriverReferenceProjectsTheWholeCatalogue(t *testing.T) {
	reference := ServiceDriverReference()
	if len(reference) != len(drivers) {
		t.Fatalf("driver reference has %d entries for %d catalogue drivers", len(reference), len(drivers))
	}
	gotNames := documentationDriverNames(reference)
	if !sort.StringsAreSorted(gotNames) {
		t.Fatalf("driver reference is not sorted: %v", gotNames)
	}
	if wantNames := DriverNames(); !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("driver names = %v, want %v", gotNames, wantNames)
	}

	for _, entry := range reference {
		d, ok := drivers[entry.Name]
		if !ok {
			t.Fatalf("documentation names unknown driver %q", entry.Name)
		}
		if entry.ImageRepository != d.image || entry.Port != d.port || entry.DataPath != d.dataPath ||
			entry.URLScheme != d.scheme || entry.HealthAvailable != (len(d.health) > 0) {
			t.Errorf("%s projection drifted from catalogue: %#v", entry.Name, entry)
		}
		if want := documentedConnectionParts(d); !reflect.DeepEqual(entry.ConnectionParts, want) {
			t.Errorf("%s connection parts = %v, want %v", entry.Name, entry.ConnectionParts, want)
		}
	}
}

func TestServiceDriverReferenceIsDefensive(t *testing.T) {
	first := ServiceDriverReference()
	if len(first) == 0 || len(first[0].ConnectionParts) == 0 {
		t.Fatal("driver reference is unexpectedly empty")
	}
	wantName := first[0].Name
	wantPart := first[0].ConnectionParts[0]
	first[0].Name = "changed"
	first[0].ConnectionParts[0] = "changed"

	second := ServiceDriverReference()
	if second[0].Name != wantName || second[0].ConnectionParts[0] != wantPart {
		t.Fatalf("caller mutation leaked into a later projection: %#v", second[0])
	}
}

func documentationDriverNames(reference []ServiceDriverDocumentation) []string {
	names := make([]string, 0, len(reference))
	for _, entry := range reference {
		names = append(names, entry.Name)
	}
	return names
}
