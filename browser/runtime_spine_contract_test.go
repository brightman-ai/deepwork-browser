package browser

import (
	"os"
	"strings"
	"testing"
)

func Test_TC_WCS_I_01_AcquireTabRuntimeContractPreservesWebChatService(t *testing.T) {
	desc := IdentityDescriptor{
		Key: IdentityKey("profile-webchat-chatgpt-pa-1")
		Profile: "webchat-chatgpt-pa-1"
		Preset: Preset{FingerprintTag: DefaultPresetID}
	}
	req := AcquireTabRequest{
		IdentityKey: desc.Key
		WorkspaceID: "provider-history-pa-1"
		Role: RoleBackground
		Mode: ModeHeaded
		BrowserSessionID: "browser-session-service-webchat-chatgpt-pa-1"
		SessionKind: SessionKindService
		Goal: "provider-history-sync"
		Owner: SessionOwnerService
		Isolation: SessionIsolationDedicated
		ServiceName: "webchat/chatgpt"
		AccountID: "pa-1"
	}

	contract := browserPoolRuntimeContractFromAcquireTab(req, desc)
	if contract.SessionKind != SessionKindService {
		t.Fatalf("session kind = %q, want service", contract.SessionKind)
	}
	if contract.BrowserSessionID != "browser-session-service-webchat-chatgpt-pa-1" {
		t.Fatalf("browser_session_id = %q", contract.BrowserSessionID)
	}
	if contract.ServiceName != "webchat/chatgpt" || contract.AccountID != "pa-1" {
		t.Fatalf("service/account not preserved: %+v", contract)
	}

	entry := &chromePoolEntry{
		identity: desc
		profileID: "webchat-chatgpt-pa-1"
		profileDir: "/tmp/profile-pa-1"
		presetID: DefaultPresetID
		mode: ModeHeaded
		runtimeContract: contract
	}
	hostReq := browserMuxHostRequestForPoolEntry(entry, "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", &FingerprintPreset{
		ViewportW: 1280
		ViewportH: 720
	}, 9222)
	if hostReq.SessionKind != SessionKindService {
		t.Fatalf("muxhost session kind = %q, want service", hostReq.SessionKind)
	}
	if hostReq.ServiceName != "webchat/chatgpt" || hostReq.AccountID != "pa-1" {
		t.Fatalf("muxhost service/account not preserved: %+v", hostReq)
	}
	if hostReq.Owner != SessionOwnerService || hostReq.Isolation != SessionIsolationDedicated {
		t.Fatalf("muxhost owner/isolation not preserved: %+v", hostReq)
	}
	if hostReq.MuxHostID != GlobalBrowserMuxHostID {
		t.Fatalf("muxhost id = %q, want global host id", hostReq.MuxHostID)
	}
	if hostReq.RuntimeID != BrowserRuntimeIDFromBrowserSessionID(contract.BrowserSessionID) {
		t.Fatalf("runtime id = %q, want browser-session derived runtime id", hostReq.RuntimeID)
	}
}

func Test_TC_WCS_I_02_AcquireTabRuntimeContractKeepsLegacyInteractiveDefault(t *testing.T) {
	desc := IdentityDescriptor{
		Key: IdentityKey("profile-human-default")
		Profile: "human-default"
		Preset: Preset{FingerprintTag: DefaultPresetID}
	}
	req := AcquireTabRequest{
		IdentityKey: desc.Key
		WorkspaceID: "browser-portal"
		Role: RoleHuman
		Mode: ModeHeaded
	}

	contract := browserPoolRuntimeContractFromAcquireTab(req, desc)
	if contract.SessionKind != SessionKindInteractive {
		t.Fatalf("default session kind = %q, want interactive", contract.SessionKind)
	}
	if contract.Owner != SessionOwnerHuman {
		t.Fatalf("default owner = %q, want human", contract.Owner)
	}
	if !strings.HasPrefix(contract.BrowserSessionID, "browser-session-pool-") {
		t.Fatalf("legacy browser_session_id = %q, want pool-derived", contract.BrowserSessionID)
	}
	if contract.BrowserSessionID != BrowserSessionIDFromPoolIdentity(desc.Key) {
		t.Fatalf("legacy browser_session_id = %q, want %q", contract.BrowserSessionID, BrowserSessionIDFromPoolIdentity(desc.Key))
	}
}

func Test_TC_WCS_L4_06_ActivePoolEntryRejectsRuntimeContractMismatch(t *testing.T) {
	existing := browserPoolRuntimeContract{
		BrowserSessionID: "browser-session-service-webchat-chatgpt-pa-1"
		SessionKind: SessionKindService
		ServiceName: "webchat/chatgpt"
		AccountID: "pa-1"
		Owner: SessionOwnerService
		Isolation: SessionIsolationDedicated
	}
	requested := existing
	requested.AccountID = "pa-2"

	if err := validateBrowserPoolRuntimeContract(existing, requested); err == nil {
		t.Fatal("expected contract mismatch error")
	}
}

func Test_TC_WCS_L4_01_MuxHostReuseRejectsServiceAccountMismatch(t *testing.T) {
	state := &BrowserMuxHostState{
		MuxHostAlive: true
		ChromeAlive: true
		BrowserSessionID: "browser-session-service-webchat-chatgpt-pa-1"
		SessionKind: SessionKindService
		ServiceName: "webchat/chatgpt"
		AccountID: "pa-1"
		DisplayBackend: "none"
		DisplayVerified: true
		ChromeWindowContained: true
	}
	req := BrowserMuxHostRequest{
		BrowserSessionID: "browser-session-service-webchat-chatgpt-pa-1"
		SessionKind: SessionKindService
		ServiceName: "webchat/chatgpt"
		AccountID: "pa-2"
		Mode: ModeHeaded
	}

	if err := validateReusableBrowserMuxHost(state, req); err == nil {
		t.Fatal("expected service account mismatch to reject muxhost reuse")
	}
}

func Test_TC_BMH_I_01_MuxHostReuseFailureClassifiesRuntimeHealthOnly(t *testing.T) {
	if !browserMuxHostRuntimeReusableFailureRecoverable(
		validateReusableBrowserMuxHost(&BrowserMuxHostState{
			MuxHostAlive: true
			ChromeAlive: true
			MuxHostPID: os.Getpid
			ChromePID: os.Getpid
			BrowserSessionID: "browser-session-pool-main"
			RuntimeID: "browser-runtime-browser-session-pool-main"
			SessionKind: SessionKindInteractive
			DisplayBackend: "cgvirtualdisplay"
			DisplayVerified: false
			ChromeWindowContained: false
		}, BrowserMuxHostRequest{
			BrowserSessionID: "browser-session-pool-main"
			RuntimeID: "browser-runtime-browser-session-pool-main"
			SessionKind: SessionKindInteractive
			Mode: ModeHeaded
		})
	) {
		t.Fatal("uncontained headed runtime should be recreated, not surfaced as identity conflict")
	}

	if browserMuxHostRuntimeReusableFailureRecoverable(
		validateReusableBrowserMuxHost(&BrowserMuxHostState{
			MuxHostAlive: true
			ChromeAlive: true
			BrowserSessionID: "browser-session-service-webchat-chatgpt-pa-1"
			RuntimeID: "browser-runtime-browser-session-service-webchat-chatgpt-pa-1"
			SessionKind: SessionKindService
			ServiceName: "webchat/chatgpt"
			AccountID: "pa-1"
			DisplayBackend: "none"
			DisplayVerified: true
			ChromeWindowContained: true
		}, BrowserMuxHostRequest{
			BrowserSessionID: "browser-session-service-webchat-chatgpt-pa-1"
			RuntimeID: "browser-runtime-browser-session-service-webchat-chatgpt-pa-1"
			SessionKind: SessionKindService
			ServiceName: "webchat/chatgpt"
			AccountID: "pa-2"
			Mode: ModeHeaded
		})
	) {
		t.Fatal("service/account mismatch must remain a hard conflict")
	}
}
