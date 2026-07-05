package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

func newApproveCmd(resolve func() (Config, error)) *cobra.Command {
	return newApproveCmdWithClient(resolve, http.DefaultClient)
}

func newApproveCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return newApprovalDecisionCmdWithClient(resolve, httpClient, "approve", "Approve a waiting approval step", "approve")
}

func newRejectCmd(resolve func() (Config, error)) *cobra.Command {
	return newRejectCmdWithClient(resolve, http.DefaultClient)
}

func newRejectCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	return newApprovalDecisionCmdWithClient(resolve, httpClient, "reject", "Reject a waiting approval step", "reject")
}

func newApprovalDecisionCmdWithClient(resolve func() (Config, error), httpClient *http.Client, use, short, decision string) *cobra.Command {
	var comment string
	cmd := &cobra.Command{
		Use:   use + " <run-id> <step-index>",
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			body, _ := json.Marshal(api.ApprovalDecisionRequest{Decision: decision, Comment: comment})
			url := cfg.Server + "/api/v1/runs/" + args[0] + "/approvals/" + args[1]
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := httpClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				b, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("server: %s (%d)", string(b), resp.StatusCode)
			}
			past := decision + "d"
			if decision == "reject" {
				past = "rejected"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s step %s of run %s\n", past, args[1], args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&comment, "comment", "", "optional comment")
	return cmd
}
