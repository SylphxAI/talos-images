package client

import (
	"context"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/bootinventory"
)

// EFIInventory requests metadata through the existing authenticated Read API.
// Stock servers do not understand this mode and refuse its empty path.
func (c *Client) EFIInventory(ctx context.Context) (bootinventory.Result, error) {
	stream, err := c.MachineClient.Read(ctx, &machineapi.ReadRequest{EfiInventory: true})
	if err != nil {
		return bootinventory.Result{}, err
	}
	r, err := ReadStream(stream)
	if err != nil {
		return bootinventory.Result{}, err
	}
	defer r.Close() //nolint:errcheck
	return bootinventory.Decode(r)
}
