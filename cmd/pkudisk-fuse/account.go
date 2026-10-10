package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"pkudisk-fuse/internal/accounts"
)

func accountCommand(args []string) error {
	command := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	set := flag.NewFlagSet("account "+command, flag.ContinueOnError)
	set.Usage = func() {
		fmt.Fprintln(set.Output(), `Usage:
  pkudisk-fuse account [list] [--json]
  pkudisk-fuse account add [authentication options] [--default] [--replace] [NAME]
  pkudisk-fuse account default NAME
  pkudisk-fuse account remove NAME

The first saved account is the default. NAME defaults to the campus username.
Passwords and sessions are saved in a private file under the user config directory.`)
		set.PrintDefaults()
	}
	var opts authFlags
	var jsonOutput bool
	var nameFlag string
	switch command {
	case "list":
		set.BoolVar(&jsonOutput, "json", false, "output account names, usernames, servers, and default status as JSON")
	case "add":
		opts.forceLogin = true
		opts.addCredentials(set)
		set.StringVar(&nameFlag, "name", "", "saved account name (defaults to the username)")
		set.BoolVar(&opts.makeDefault, "default", false, "make this account the default")
		set.BoolVar(&opts.replace, "replace", false, "replace an existing saved account after successful login")
	case "default", "remove":
	default:
		set.Usage()
		return errors.New("unknown account command")
	}
	if err := set.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if command == "add" {
		if set.NArg() > 1 || (set.NArg() == 1 && nameFlag != "") {
			return errors.New("account add takes at most one account name; place options before it")
		}
		if set.NArg() == 1 {
			opts.saveName = set.Arg(0)
		} else {
			opts.saveName = nameFlag
		}
		s, _, _, err := authenticate(context.Background(), opts)
		if s != nil {
			s.Close()
		}
		return err
	}
	if (command == "list" && set.NArg() != 0) || (command != "list" && command != "add" && set.NArg() != 1) {
		return errors.New("account list takes no arguments; account default/remove require one account name")
	}
	store, err := accounts.Default()
	if err != nil {
		return err
	}
	switch command {
	case "default":
		if err := store.SetDefault(set.Arg(0)); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Default account: %s\n", set.Arg(0))
		return nil
	case "remove":
		if err := store.Remove(set.Arg(0)); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Removed account: %s\n", set.Arg(0))
		return nil
	}
	config, err := store.Load()
	if err != nil {
		return err
	}
	return printAccounts(config, jsonOutput)
}

func printAccounts(config accounts.Config, jsonOutput bool) error {
	// Build a separate public view: neither text nor JSON can expose credentials.
	type summary struct {
		Name     string `json:"name"`
		Username string `json:"username"`
		Server   string `json:"server"`
		Default  bool   `json:"default"`
	}
	rows := make([]summary, 0, len(config.Accounts))
	for _, a := range config.Accounts {
		rows = append(rows, summary{Name: a.Name, Username: a.Username, Server: a.Server, Default: a.Name == config.Default})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	if jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stdout, "No saved accounts. Run 'account add' or mount to log in and save the first account.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "DEFAULT\tNAME\tUSERNAME\tSERVER")
	for _, row := range rows {
		marker := ""
		if row.Default {
			marker = "*"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", marker, row.Name, row.Username, row.Server)
	}
	return w.Flush()
}
