package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/brightman-ai/deepwork-browser/browser"
)

func TestAcquisitionCLIExplicitDNSResolverAndDefault(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		explicit    bool
	}{
		{want: "system"}, {input: "system", want: "system", explicit: true},
		{input: "https://1.1.1.1:443/dns-query", want: "https://1.1.1.1/dns-query", explicit: true},
		{input: "https://[2606:4700:4700::1111]/dns-query", want: "https://[2606:4700:4700::1111]/dns-query", explicit: true},
	} {
		docArgs := []string{"https://example.com/"}
		fetchArgs := []string{"https://example.com/", "--save", "body"}
		if tc.explicit {
			docArgs = append(docArgs, "--dns-resolver", tc.input)
			fetchArgs = append(fetchArgs, "--dns-resolver="+tc.input)
		}
		doc, err := parseDocumentRequest(docArgs)
		if err != nil || doc.DNSResolver != tc.want || doc.NetworkRoute != browser.NetworkRouteSystem {
			t.Fatalf("document resolver %+v: got %q err=%v", tc, doc.DNSResolver, err)
		}
		resource, _, err := parseResourceRequest(fetchArgs)
		if err != nil || resource.DNSResolver != tc.want || resource.NetworkRoute != browser.NetworkRouteSystem {
			t.Fatalf("fetch resolver %+v: got %q err=%v", tc, resource.DNSResolver, err)
		}
	}
}

func TestAcquisitionCLIRejectsInvalidOrDuplicateDNSResolver(t *testing.T) {
	for _, value := range []string{"", "auto", "http://1.1.1.1/dns-query", "https://dns.example/dns-query", "https://127.0.0.1/dns-query", "https://10.0.0.1/dns-query", "https://1.1.1.1:444/dns-query", "https://1.1.1.1/", "https://1.1.1.1/dns-query?x=1", "https://1.1.1.1/dns-query#x", "https://user@1.1.1.1/dns-query"} {
		if _, err := parseDocumentRequest([]string{"https://example.com/", "--dns-resolver=" + value}); err == nil {
			t.Fatalf("document accepted invalid resolver %q", value)
		}
		if _, _, err := parseResourceRequest([]string{"https://example.com/", "--save", "body", "--dns-resolver", value}); err == nil {
			t.Fatalf("fetch accepted invalid resolver %q", value)
		}
	}
	for _, command := range []string{"document", "fetch"} {
		argv := []string{command, "https://example.com/", "--dns-resolver=system", "--dns-resolver", "https://1.1.1.1/dns-query"}
		if command == "fetch" {
			argv = append(argv, "--save", "body")
		}
		if err := validateDeclaredRequest(argv); err == nil {
			t.Fatalf("duplicate resolver accepted: %q", argv)
		}
	}
}

func TestDNSResolverAvailableOnlyOnAcquisitionCommands(t *testing.T) {
	for _, definition := range commandDefinitions() {
		present := false
		for _, flag := range definition.Flags {
			if flag.Name == "--dns-resolver" {
				present = true
			}
		}
		allowed := definition.Path == "fetch" || definition.Path == "document"
		if present != allowed {
			t.Fatalf("resolver flag scope changed: %s present=%v", definition.Path, present)
		}
	}
	for _, argv := range [][]string{
		{"open", "https://example.com/", "--id", "task", "--scenario", "webvisit", "--dns-resolver", "system"},
		{"observe", "--id", "task", "--dns-resolver", "system"},
	} {
		if err := validateDeclaredRequest(argv); err == nil {
			t.Fatalf("non-acquisition command accepted resolver: %q", argv)
		}
	}
}

func TestDNSResolverHelpExplainsCallerChoiceAndPrivacy(t *testing.T) {
	for _, command := range []string{"fetch", "document"} {
		data, err := json.Marshal(generatedHelp(command))
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"--dns-resolver", "system", "literal public IP", "IP SAN", "per call", "never switch after a timeout", "network-byte budget", "not the original URL path"} {
			if !strings.Contains(string(data), required) {
				t.Fatalf("%s help omits %q: %s", command, required, data)
			}
		}
	}
	if !strings.Contains(generatedProtocol(), "Only use that endpoint when the caller explicitly chooses") {
		t.Fatal("protocol fails to require explicit caller resolver strategy")
	}
}
