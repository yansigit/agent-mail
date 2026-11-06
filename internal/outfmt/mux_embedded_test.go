package outfmt

import (
	"encoding/json"
	"testing"
)

// mailmux results embed upstream result structs to add fields such as
// account. These tests pin that embedding keeps both halves of the output
// contract: untrusted fields inside the embedded struct are still wrapped, and
// the JSON stays flat. The embedded type must be exported, as upstream's
// result types are: see TestMuxEmbedded_UnexportedEmbedIsNotWrapped.

type MuxInner struct {
	ID      string `json:"id"`
	Subject string `json:"subject" untrusted:"true"`
}

type muxByValue struct {
	MuxInner
	Account string `json:"account"`
}

type muxByPointer struct {
	*MuxInner
	Account string `json:"account"`
}

const muxWrappedHi = "[UNTRUSTED:abcd]hi[/UNTRUSTED:abcd]"

func TestMuxEmbedded_WrapsEmbeddedUntrustedFields(t *testing.T) {
	in := muxByValue{MuxInner{ID: "1", Subject: "hi"}, "a@x"}
	out, ok := wrapUntrusted(in, "abcd").(muxByValue)
	if !ok {
		t.Fatalf("wrapUntrusted returned %T, want muxByValue", out)
	}
	if out.Subject != muxWrappedHi {
		t.Errorf("Subject = %q, want %q", out.Subject, muxWrappedHi)
	}
	if out.ID != "1" || out.Account != "a@x" {
		t.Errorf("trusted fields changed: ID=%q Account=%q", out.ID, out.Account)
	}
	if in.Subject != "hi" {
		t.Errorf("original mutated: Subject = %q", in.Subject)
	}
}

func TestMuxEmbedded_WrapsPointerEmbeddedFields(t *testing.T) {
	in := muxByPointer{&MuxInner{ID: "1", Subject: "hi"}, "a@x"}
	out, ok := wrapUntrusted(in, "abcd").(muxByPointer)
	if !ok {
		t.Fatalf("wrapUntrusted returned %T, want muxByPointer", out)
	}
	if out.Subject != muxWrappedHi {
		t.Errorf("Subject = %q, want %q", out.Subject, muxWrappedHi)
	}
	if out.ID != "1" || out.Account != "a@x" {
		t.Errorf("trusted fields changed: ID=%q Account=%q", out.ID, out.Account)
	}
	if in.Subject != "hi" {
		t.Errorf("original mutated: Subject = %q", in.Subject)
	}
}

func TestMuxEmbedded_JSONIsFlat(t *testing.T) {
	data, err := json.Marshal(muxByValue{MuxInner{ID: "1", Subject: "hi"}, "a@x"})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"1","subject":"hi","account":"a@x"}`
	if string(data) != want {
		t.Errorf("JSON = %s, want %s", data, want)
	}
}

// muxHidden is embedded unexported. transformUntrusted skips fields with a
// PkgPath, and an unexported embedded field has one, so its untrusted fields
// are NOT wrapped, while encoding/json still prints them as promoted fields.
// mailmux wrapper types must therefore embed exported types only.
type muxHidden struct {
	Subject string `json:"subject" untrusted:"true"`
}

type muxWithHidden struct {
	muxHidden
	Account string `json:"account"`
}

func TestMuxEmbedded_UnexportedEmbedIsNotWrapped(t *testing.T) {
	out, ok := wrapUntrusted(muxWithHidden{muxHidden{Subject: "hi"}, "a@x"}, "abcd").(muxWithHidden)
	if !ok {
		t.Fatalf("wrapUntrusted returned %T, want muxWithHidden", out)
	}
	if out.Subject != "hi" {
		t.Errorf("Subject = %q; unexported embeds are now wrapped, so the M1 rule "+
			"\"embed exported types only\" can be relaxed and this test updated", out.Subject)
	}
}
