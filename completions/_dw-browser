#compdef dw-browser
_dw_browser_v2() {
 local -a candidates
 if (( CURRENT == 2 )); then candidates=(act audit capabilities check close completion cookie-import diff document eval excerpt export-contract fetch gc help htr journey muxhost observe once open persona profile protocol record session skills tabs version wait); _describe 'command' candidates; return; fi
 case "${words[2]} ${words[3]}" in
 'session status') candidates=('--id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'session list') candidates=('--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'session viewport') candidates=('--id' '--size' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'tabs list') candidates=('--id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'tabs select') candidates=('--id' '--target' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'tabs close') candidates=('--id' '--target' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'tabs new') candidates=('--id' '--url' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'htr status') candidates=('--id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'htr takeover') candidates=('--id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'htr yield') candidates=('--id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'htr share') candidates=('--id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'persona list') candidates=('--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'profile list') candidates=('--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'profile import') candidates=('--name' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'profile prune') candidates=('--min-age' '--ephemeral' '--dry-run' '--reap-orphaned' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'skills list') candidates=('--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'skills read') candidates=('--action' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'skills write') candidates=('--action' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'record start') candidates=('--id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'record stop') candidates=('--id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'record export') candidates=('--id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'muxhost serve') candidates=('--muxhost-id' '--runtime-id' '--browser-session-id' '--kind' '--profile-id' '--profile-dir' '--chrome-path' '--debug-port' '--owner-pid' '--owner' '--isolation' '--mode' '--goal' '--idle-ttl' '--persona' '--preset' '--width' '--height' '--user-agent' '--service' '--account' '--identity-key' '--touch' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'muxhost ensure') candidates=('--muxhost-id' '--runtime-id' '--browser-session-id' '--kind' '--profile-id' '--profile-dir' '--chrome-path' '--debug-port' '--owner-pid' '--owner' '--isolation' '--mode' '--goal' '--idle-ttl' '--persona' '--preset' '--width' '--height' '--user-agent' '--service' '--account' '--identity-key' '--touch' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'muxhost status') candidates=('--muxhost-id' '--runtime-id' '--browser-session-id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'muxhost release') candidates=('--muxhost-id' '--runtime-id' '--browser-session-id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'muxhost shutdown') candidates=('--muxhost-id' '--runtime-id' '--browser-session-id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act click') candidates=('--id' '--x' '--y' '--unit' '--frame' '--observation-id' '--button' '--count' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act tap') candidates=('--id' '--x' '--y' '--unit' '--frame' '--observation-id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act hover') candidates=('--id' '--x' '--y' '--unit' '--frame' '--observation-id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act fill') candidates=('--id' '--text' '--text-file' '--sensitive' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act type') candidates=('--id' '--text' '--text-file' '--focused' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act press') candidates=('--id' '--key' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act focus') candidates=('--id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act select') candidates=('--id' '--label' '--value' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act set-checked') candidates=('--id' '--checked' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act scroll') candidates=('--id' '--x' '--y' '--unit' '--frame' '--observation-id' '--direction' '--steps' '--delta-x' '--delta-y' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act scrollto') candidates=('--id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act drag') candidates=('--id' '--x' '--y' '--unit' '--frame' '--observation-id' '--end-x' '--end-y' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act swipe') candidates=('--id' '--x' '--y' '--unit' '--frame' '--observation-id' '--end-x' '--end-y' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act navigate') candidates=('--id' '--url' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act reload') candidates=('--id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act back') candidates=('--id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act forward') candidates=('--id' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act zoom') candidates=('--id' '--value' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 'act keyboard') candidates=('--id' '--value' '--output' '--lock-wait' '--verbose' ); _describe 'flag' candidates; return;;
 esac
 case "${words[2]}" in
 'act') candidates=('click' 'tap' 'hover' 'fill' 'type' 'press' 'focus' 'select' 'set-checked' 'scroll' 'scrollto' 'drag' 'swipe' 'navigate' 'reload' 'back' 'forward' 'zoom' 'keyboard' );;
 'audit') candidates=('--id' '--suite' '--check' '--threshold' '--format' '--output' '--lock-wait' '--verbose' );;
 'capabilities') candidates=('--output' '--lock-wait' '--verbose' );;
 'check') candidates=('--id' '--observation' '--assert' '--file' '--using' '--vision-url' '--vision-provider' '--vision-model' '--vision-api-key' '--output' '--lock-wait' '--verbose' );;
 'close') candidates=('--id' '--force' '--output' '--lock-wait' '--verbose' );;
 'completion') candidates=('--output' '--lock-wait' '--verbose' );;
 'cookie-import') candidates=('--id' '--browser' '--domain' '--output' '--lock-wait' '--verbose' );;
 'diff') candidates=('--output' '--lock-wait' '--verbose' );;
 'document') candidates=('--timeout' '--max-html-bytes' '--max-network-bytes' '--network-route' '--dns-resolver' '--mode' '--output' '--lock-wait' '--verbose' );;
 'eval') candidates=('--id' '--output' '--lock-wait' '--verbose' );;
 'excerpt') candidates=('--id' '--spec' '--out' '--settle' '--output' '--lock-wait' '--verbose' );;
 'export-contract') candidates=('--directory' '--output' '--lock-wait' '--verbose' );;
 'fetch') candidates=('--save' '--timeout' '--max-bytes' '--max-network-bytes' '--network-route' '--dns-resolver' '--output' '--lock-wait' '--verbose' );;
 'gc') candidates=('--older-than' '--dry-run' '--all' '--output' '--lock-wait' '--verbose' );;
 'help') candidates=('--output' '--lock-wait' '--verbose' );;
 'htr') candidates=('status' 'takeover' 'yield' 'share' );;
 'journey') candidates=('--scenario' '--mode' '--persona' '--engine' '--safari-device' '--viewport' '--profile' '--evidence' '--allow-host' '--browser-chrome' '--user-agent' '--goal' '--stealth' '--id' '--file' '--base-url' '--fail-fast' '--output' '--lock-wait' '--verbose' );;
 'muxhost') candidates=('serve' 'ensure' 'status' 'release' 'shutdown' );;
 'observe') candidates=('--id' '--screenshot' '--content' '--scope' '--top' '--budget' '--health' '--tree' '--all' '--hit-audit' '--annotate' '--output' '--lock-wait' '--verbose' );;
 'once') candidates=('--scenario' '--mode' '--persona' '--engine' '--safari-device' '--viewport' '--profile' '--evidence' '--allow-host' '--browser-chrome' '--user-agent' '--goal' '--stealth' '--screenshot' '--content' '--health' '--tree' '--all' '--annotate' '--output' '--lock-wait' '--verbose' );;
 'open') candidates=('--scenario' '--mode' '--persona' '--engine' '--safari-device' '--viewport' '--profile' '--evidence' '--allow-host' '--browser-chrome' '--user-agent' '--goal' '--stealth' '--id' '--output' '--lock-wait' '--verbose' );;
 'persona') candidates=('list' );;
 'profile') candidates=('list' 'import' 'prune' );;
 'protocol') candidates=('--output' '--lock-wait' '--verbose' );;
 'record') candidates=('start' 'stop' 'export' );;
 'session') candidates=('status' 'list' 'viewport' );;
 'skills') candidates=('list' 'read' 'write' );;
 'tabs') candidates=('list' 'select' 'close' 'new' );;
 'version') candidates=('--output' '--lock-wait' '--verbose' );;
 'wait') candidates=('--id' '--assert' '--timeout' '--duration' '--output' '--lock-wait' '--verbose' );;
 esac
 _describe 'argument' candidates
}
compdef _dw_browser_v2 dw-browser
