package onebox

import (
	"bytes"
	"errors"
	"testing"
)

func TestActiveVolumeRecordEncodeDecode(t *testing.T) {
	previous := &ActiveVolumeSelection{DockerVolume: "ob-example-database-data", OperationID: "seed-existing", Epoch: 1}
	record, err := NewActiveVolumeRecord(
		"example", "production", "database", "data", "ob-example-database-restore-7", "restore-cutover-7", 2, previous,
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeActiveVolumeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeActiveVolumeRecord(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.SelectedVolume != "ob-example-database-restore-7" || decoded.PreviousSelection == nil || decoded.PreviousSelection.DockerVolume != "ob-example-database-data" {
		t.Fatalf("decoded active volume = %#v", decoded)
	}
	if decoded.RecordDigest != record.RecordDigest {
		t.Fatal("active-volume digest changed across encode/decode")
	}
}

func TestActiveVolumeRecordDetectsTamper(t *testing.T) {
	record, err := NewActiveVolumeRecord("example", "production", "database", "data", "ob-example-database-data", "seed-existing", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeActiveVolumeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(encoded, []byte("ob-example-database-data"), []byte("ob-example-database-evil"), 1)
	if _, err := DecodeActiveVolumeRecord(tampered); err == nil {
		t.Fatal("tampered active-volume record was accepted")
	}
}

func TestActiveVolumeRecordReportsMissingState(t *testing.T) {
	for _, encoded := range [][]byte{nil, {}, []byte(" \n\t")} {
		if _, err := DecodeActiveVolumeRecord(encoded); !errors.Is(err, ErrActiveVolumeStateMissing) {
			t.Fatalf("decode missing state error = %v", err)
		}
	}
}

func TestActiveVolumeRecordRejectsStaleEpoch(t *testing.T) {
	record, err := NewActiveVolumeRecord("example", "production", "database", "data", "ob-example-database-data", "seed-existing", 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := record.ValidateEpoch(4); !errors.Is(err, ErrActiveVolumeStaleEpoch) {
		t.Fatalf("stale epoch error = %v", err)
	}
	if err := record.ValidateEpoch(3); err != nil {
		t.Fatalf("current epoch rejected: %v", err)
	}
}

func TestActiveVolumePreviousSelectionMustBeOlderAndDifferent(t *testing.T) {
	previous := &ActiveVolumeSelection{DockerVolume: "ob-example-database-data", OperationID: "old", Epoch: 2}
	if _, err := NewActiveVolumeRecord("example", "production", "database", "data", "ob-example-database-data", "new", 2, previous); err == nil {
		t.Fatal("same-volume, same-epoch previous selection was accepted")
	}
}
