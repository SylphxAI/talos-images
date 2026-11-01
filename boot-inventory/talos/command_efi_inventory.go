package talos

import (
	"encoding/json"
	"os"

	"github.com/spf13/cobra"
)

var efiInventoryCmd = &cobra.Command{
	Use: "efi-inventory", Short: "Read installed EFI filenames, sizes, hashes and boot identity", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		factory, err := NewClientFactory(cmd.Context(), nil)
		if err != nil {
			return err
		}
		defer factory.Close() //nolint:errcheck
		ctx, c, _, err := factory.BuildClientEnforceSingleNode(cmd.Context(), "efi-inventory")
		if err != nil {
			return err
		}
		result, err := c.EFIInventory(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result) //nolint:forbidigo
	},
}

func init() { addCommand(efiInventoryCmd) }
