package engine

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"testing"
)

func TestSecurityCatalogPreservesFloorAndLegacy(t *testing.T) {
	for raw, want := range map[string]string{string(detectorsDefault): "97af41423034fb77ef4031a93b9cf4e2f1809648445d3de828208436429d9a01", string(detectorsStructural): "daba3b73ade88fe7d2033c4582ded3c07b3ad40ed31023fa0ec3c8a9d8103ac5"} {
		if fmt.Sprintf("%x", sha256.Sum256([]byte(raw))) != want {
			t.Fatal("compatibility bytes changed")
		}
	}
	base, err := StructuralDetectors()
	if err != nil {
		t.Fatal(err)
	}
	secure, err := SecurityObserveDetectors()
	if err != nil {
		t.Fatal(err)
	}
	if len(secure) != 35 || !reflect.DeepEqual(base, secure[:30]) {
		t.Fatal("security must contain exact floor plus five additions")
	}
	want := map[string]bool{"net.dest": true, "secret.aws-key": true, "secret.private-key": true, "secret.gh-token": true, "secret.bearer": true}
	for _, d := range secure[30:] {
		if !want[d.ID] {
			t.Fatal(d.ID)
		}
		delete(want, d.ID)
	}
	if len(want) != 0 {
		t.Fatal(want)
	}
}
