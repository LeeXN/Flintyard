package state

import (
	"path/filepath"
	"testing"
)

func TestJournalRecovery(t *testing.T) {
	p := filepath.Join(t.TempDir(), "journal.json")
	j, e := NewJournal(p, "owner")
	if e != nil {
		t.Fatal(e)
	}
	id, e := j.Intent("netns")
	if e != nil {
		t.Fatal(e)
	}
	got, e := ReadJournal(p, "owner")
	if e != nil || got.Resources[0].State != "intent" {
		t.Fatal(got, e)
	}
	if e = j.Released(id); e == nil {
		t.Fatal("released unobserved resource")
	}
	identity := map[string]string{"inode": "123", "owner": "owner"}
	if e = j.Done(id, identity); e != nil {
		t.Fatal(e)
	}
	identity["inode"] = "tampered"
	if j.Resources[0].Identity["inode"] != "123" {
		t.Fatal("aliased caller state")
	}
	if _, e = ReadJournal(p, "other"); e == nil {
		t.Fatal("accepted foreign journal")
	}
	if e = j.Released(id); e != nil {
		t.Fatal(e)
	}
	if e = j.Released(id); e != nil {
		t.Fatal(e)
	}
}

func TestJournalIntentIdentityAndObservedAbsence(t *testing.T) {
	p := filepath.Join(t.TempDir(), "journal.json")
	j, err := NewJournal(p, "owner")
	if err != nil {
		t.Fatal(err)
	}
	desired := map[string]string{"name": "exact-name", "owner": "uuid-owner"}
	id, err := j.IntentIdentity("netns", desired)
	if err != nil {
		t.Fatal(err)
	}
	desired["owner"] = "changed"
	reloaded, err := ReadJournal(p, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Resources[0].Identity["owner"] != "uuid-owner" {
		t.Fatal("intent identity not snapshotted")
	}
	if err = reloaded.Absent(id); err != nil {
		t.Fatal(err)
	}
	if reloaded.Resources[0].State != "released" || reloaded.Absent(id) == nil {
		t.Fatal("invalid absence transition")
	}
}
