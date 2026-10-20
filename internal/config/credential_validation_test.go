package config

import "testing"

func TestValidationLoadResolvesAllWithoutReinterpretingSecret(t *testing.T) {
	t.Setenv("MAILVAULT_LITERAL_PREFIX_FIXTURE", "env:MAILVAULT_NOT_ANOTHER_REFERENCE")
	path := credentialConfigFile(t, "env:MAILVAULT_LITERAL_PREFIX_FIXTURE", "")
	normal, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := LoadForValidation(path)
	if err != nil {
		t.Fatal(err)
	}
	if normal.Sources[1].PasswordRef != "env:MAILVAULT_LITERAL_PREFIX_FIXTURE" {
		t.Fatal("normal load resolved unused source")
	}
	if validated.Sources[1].PasswordRef != "env:MAILVAULT_NOT_ANOTHER_REFERENCE" {
		t.Fatal("validation reinterpreted expanded password as another reference")
	}
	if normal.Sources[1].PasswordRef != "env:MAILVAULT_LITERAL_PREFIX_FIXTURE" {
		t.Fatal("validation mutated previously loaded configuration")
	}
}
