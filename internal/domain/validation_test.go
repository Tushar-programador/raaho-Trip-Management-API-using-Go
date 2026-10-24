package domain

import (
	"errors"
	"strings"
	"testing"
)

func validInput() CreateTripInput {
	return CreateTripInput{TripNumber: "TRIP-1", Source: "Mumbai", Destination: "Pune", DriverName: "Ravi"}
}

func TestCreateTripInput_Validate(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*CreateTripInput)
		wantFields []string
	}{
		{"valid", func(*CreateTripInput) {}, nil},
		{"missing trip_number", func(in *CreateTripInput) { in.TripNumber = "" }, []string{"trip_number"}},
		{"whitespace-only source", func(in *CreateTripInput) { in.Source = " \t\n " }, []string{"source"}},
		{"all missing", func(in *CreateTripInput) { *in = CreateTripInput{} }, []string{"trip_number", "source", "destination", "driver_name"}},
		{"trip_number at limit", func(in *CreateTripInput) { in.TripNumber = strings.Repeat("a", MaxTripNumberLength) }, nil},
		{"trip_number too long", func(in *CreateTripInput) { in.TripNumber = strings.Repeat("a", MaxTripNumberLength+1) }, []string{"trip_number"}},
		{"driver_name too long", func(in *CreateTripInput) { in.DriverName = strings.Repeat("a", MaxDriverNameLength+1) }, []string{"driver_name"}},
		// Length is counted in characters (matching VARCHAR), not bytes.
		{"multibyte at limit", func(in *CreateTripInput) { in.DriverName = strings.Repeat("é", MaxDriverNameLength) }, nil},
		{"destination too long", func(in *CreateTripInput) { in.Destination = strings.Repeat("x", MaxDestinationLength+1) }, []string{"destination"}},
		{"NUL byte", func(in *CreateTripInput) { in.Source = "Mum\x00bai" }, []string{"source"}},
		{"embedded newline", func(in *CreateTripInput) { in.DriverName = "Ravi\nKumar" }, []string{"driver_name"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := validInput()
			c.mutate(&in)
			err := in.Normalize().Validate()

			if c.wantFields == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want *ValidationError, got %v", err)
			}
			if len(ve.Fields) != len(c.wantFields) {
				t.Fatalf("fields = %+v, want %v", ve.Fields, c.wantFields)
			}
			for i, f := range c.wantFields {
				if ve.Fields[i].Field != f {
					t.Errorf("field %d = %q, want %q", i, ve.Fields[i].Field, f)
				}
			}
		})
	}
}

func TestNormalizeTrims(t *testing.T) {
	got := CreateTripInput{TripNumber: "  T-1 ", Source: "\tA", Destination: "B\n", DriverName: " C "}.Normalize()
	want := CreateTripInput{TripNumber: "T-1", Source: "A", Destination: "B", DriverName: "C"}
	if got != want {
		t.Errorf("Normalize() = %+v, want %+v", got, want)
	}
}

func TestFingerprint(t *testing.T) {
	a := validInput()
	b := CreateTripInput{TripNumber: " TRIP-1 ", Source: "Mumbai", Destination: "Pune", DriverName: "Ravi"}.Normalize()
	if a.Fingerprint() != b.Fingerprint() {
		t.Error("equal normalized inputs must have equal fingerprints")
	}
	c := a
	c.Destination = "Nashik"
	if a.Fingerprint() == c.Fingerprint() {
		t.Error("different inputs must have different fingerprints")
	}
	// Field boundaries must matter: moving characters between fields changes the hash.
	d := CreateTripInput{TripNumber: "TRIP-1M", Source: "umbai", Destination: "Pune", DriverName: "Ravi"}
	if a.Fingerprint() == d.Fingerprint() {
		t.Error("fingerprint must not be a plain concatenation of fields")
	}
	if len(a.Fingerprint()) != 64 {
		t.Errorf("fingerprint length = %d, want 64 hex chars", len(a.Fingerprint()))
	}
}

func TestValidateClientName(t *testing.T) {
	for _, ok := range []string{"mobile-app", "Billing Service", strings.Repeat("é", MaxClientNameLength)} {
		if err := ValidateClientName(ok); err != nil {
			t.Errorf("ValidateClientName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("a", MaxClientNameLength+1), "line\nbreak", "nul\x00"} {
		if err := ValidateClientName(bad); err == nil {
			t.Errorf("ValidateClientName(%q) accepted an invalid name", bad)
		}
	}
}

func TestValidateIdempotencyKey(t *testing.T) {
	valid := []string{"a", "0192f3a4-5b6c-7d8e-9f01-23456789abcd", strings.Repeat("k", MaxIdempotencyKeyLength), "key_with.symbols~!"}
	for _, k := range valid {
		if err := ValidateIdempotencyKey(k); err != nil {
			t.Errorf("ValidateIdempotencyKey(%q) = %v", k, err)
		}
	}
	invalid := []string{"", " ", "has space", strings.Repeat("k", MaxIdempotencyKeyLength+1), "tab\tkey", "ключ"}
	for _, k := range invalid {
		if err := ValidateIdempotencyKey(k); err == nil {
			t.Errorf("ValidateIdempotencyKey(%q) accepted an invalid key", k)
		}
	}
}
