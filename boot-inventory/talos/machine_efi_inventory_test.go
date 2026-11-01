package machine

import (
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestEFIInventoryWireMode(t *testing.T) {
	request := &ReadRequest{EfiInventory: true}
	for name, encode := range map[string]func() ([]byte, error){"protobuf": func() ([]byte, error) { return proto.Marshal(request) }, "vtprotobuf": request.MarshalVT} {
		t.Run(name, func(t *testing.T) {
			wire, err := encode()
			if err != nil {
				t.Fatal(err)
			}
			for _, decode := range []func([]byte, *ReadRequest) error{func(b []byte, r *ReadRequest) error { return proto.Unmarshal(b, r) }, func(b []byte, r *ReadRequest) error { return r.UnmarshalVT(b) }} {
				var got ReadRequest
				if err := decode(wire, &got); err != nil || !got.GetEfiInventory() || got.Path != "" {
					t.Fatalf("inventory mode lost: %+v %v", got, err)
				}
			}
		})
	}
	old := &ReadRequest{Path: "/old/regular-file"}
	wire, err := old.MarshalVT()
	if err != nil {
		t.Fatal(err)
	}
	var got ReadRequest
	if err = proto.Unmarshal(wire, &got); err != nil || got.GetEfiInventory() || got.Path != old.Path {
		t.Fatal("legacy Read wire behavior changed")
	}
}
