package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

// newAuthzCmd returns the command group for inspecting the authorization policy.
func newAuthzCmd(resolve func() (Config, error)) *cobra.Command {
	return newAuthzCmdWithClient(resolve, http.DefaultClient)
}

func newAuthzCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	cmd := &cobra.Command{Use: "authz", Short: "Inspect the authorization policy (requires authz.read)"}
	cmd.AddCommand(newAuthzShowCmd(resolve, httpClient), newAuthzExplainCmd(resolve, httpClient))
	return cmd
}

// checkOutputFormat rejects anything but table, yaml and json before any
// request is made.
func checkOutputFormat(format string) error {
	switch format {
	case "table", "yaml", "json":
		return nil
	}
	return fmt.Errorf("unsupported output format %q (use table, yaml or json)", format)
}

// authzCall sends one request with the CLI token and returns the response body.
// A non-200 answer is returned as `server error: <body>`.
func authzCall(resolve func() (Config, error), httpClient *http.Client, method, path string, body any) ([]byte, error) {
	cfg, err := resolve()
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, cfg.Server+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server error: %s", string(b))
	}
	return b, nil
}

// printStructured writes the server's JSON as indented JSON or as YAML.
func printStructured(w io.Writer, format string, raw []byte) error {
	if format == "yaml" {
		out, err := yaml.JSONToYAML(raw)
		if err != nil {
			return err
		}
		_, err = w.Write(out)
		return err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return err
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}

func newAuthzShowCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show the authorization policy as the controller resolved it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutputFormat(output); err != nil {
				return err
			}
			raw, err := authzCall(resolve, httpClient, http.MethodGet, "/api/v1/authz/policy", nil)
			if err != nil {
				return err
			}
			if output != "table" {
				return printStructured(cmd.OutOrStdout(), output, raw)
			}
			var p api.AuthzPolicy
			if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			printPolicyTable(cmd.OutOrStdout(), p)
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "table", "output format: table, yaml or json")
	return cmd
}

func joinOrDash(items []string) string {
	return dashIfEmpty(strings.Join(items, ","))
}

func printPolicyTable(w io.Writer, p api.AuthzPolicy) {
	row := func(subject, name, grant, perms string) {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", subject, name, dashIfEmpty(grant), perms)
	}
	section := func(title string) {
		fmt.Fprintf(w, "\n%s\n", title)
		row("SUBJECT", "NAME", "GRANT", "PERMISSIONS")
	}

	fmt.Fprintf(w, "oidc\t%t\n", p.OIDC)
	if p.OIDC {
		fmt.Fprintf(w, "rolesClaim\t%s\n", p.RolesClaim)
	}

	section("BUNDLES")
	for _, name := range []string{"admin", "developer", "viewer"} {
		if perms, ok := p.Bundles[name]; ok {
			row("role", name, "", joinOrDash(perms))
		}
	}

	if p.OIDC {
		section("LEGACY")
		legacy := func(subject string, e api.AuthzLegacyEntry) {
			role := e.Role
			if e.Ignored {
				role += " (ignored)"
			}
			row(subject, e.Key, role, joinOrDash(e.Permissions))
		}
		for _, e := range p.Legacy.RoleMap {
			legacy("roleMap", e)
		}
		for _, e := range p.Legacy.UserMap {
			legacy("userMap", e)
		}
		legacy("defaultRole", p.Legacy.DefaultRole)
	}

	entries := func(list []api.AuthzGrantEntry) {
		for _, e := range list {
			row(e.Subject, e.Name, strings.Join(e.Grant, ","), joinOrDash(e.Permissions))
		}
	}
	if len(p.Global) > 0 {
		section("GLOBAL")
		entries(p.Global)
	}
	for _, f := range p.Folders {
		section("FOLDER " + f.Folder)
		entries(f.Entries)
	}
}

func newAuthzExplainCmd(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	var (
		output, user, job, folder string
		groups                    []string
	)
	cmd := &cobra.Command{
		Use:   "explain",
		Short: "Explain the effective permissions of a user and/or groups",
		Long: "Explain which permissions an identity holds at a scope and which grants give them.\n" +
			"--user is matched as both email and sub. --group is repeatable; when given it is used\n" +
			"as the group list, otherwise the groups of the user's latest browser login are used.\n" +
			"A PAT's ceiling is not applied: this explains an identity, not a token.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutputFormat(output); err != nil {
				return err
			}
			req := api.AuthzExplainRequest{User: user, Job: job, Folder: folder}
			if cmd.Flags().Changed("group") {
				req.Groups = &groups
			}
			raw, err := authzCall(resolve, httpClient, http.MethodPost, "/api/v1/authz/explain", req)
			if err != nil {
				return err
			}
			if output != "table" {
				return printStructured(cmd.OutOrStdout(), output, raw)
			}
			var resp api.AuthzExplainResponse
			if err := json.Unmarshal(raw, &resp); err != nil {
				return err
			}
			printExplainTable(cmd.OutOrStdout(), resp)
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "table", "output format: table, yaml or json")
	cmd.Flags().StringVar(&user, "user", "", "user to explain (email or sub)")
	cmd.Flags().StringArrayVar(&groups, "group", nil, "group of the identity (repeatable)")
	cmd.Flags().StringVar(&job, "job", "", "explain at this job (qualified name)")
	cmd.Flags().StringVar(&folder, "folder", "", "explain at this folder, as a job directly inside it")
	cmd.MarkFlagsMutuallyExclusive("job", "folder")
	return cmd
}

func printExplainTable(w io.Writer, r api.AuthzExplainResponse) {
	scope := r.Scope.Kind
	if r.Scope.Name != "" {
		scope += " " + r.Scope.Name
	}
	fmt.Fprintf(w, "identity\t%s\n", dashIfEmpty(r.Identity.User))
	fmt.Fprintf(w, "groups (%s)\t%s\n", r.Identity.GroupsSource, joinOrDash(r.Identity.Groups))
	fmt.Fprintf(w, "scope\t%s\n", scope)
	fmt.Fprintf(w, "global role\t%s\n", dashIfEmpty(r.GlobalRole))
	fmt.Fprintf(w, "login allowed\t%t\n", r.LoginAllowed)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "PERMISSION\tGRANTED BY")
	for _, p := range r.Permissions {
		fmt.Fprintf(w, "%s\t%s\n", p.Permission, strings.Join(p.GrantedBy, "; "))
	}
}
