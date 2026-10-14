_dw_browser_v2() {
 local cur="${COMP_WORDS[COMP_CWORD]}" prev="${COMP_WORDS[COMP_CWORD-1]}" choices=""
 if (( COMP_CWORD == 1 )); then choices='act audit capabilities check close completion cookie-import diff eval export-contract gc help htr journey muxhost observe once open persona profile protocol record session skills tabs version wait'; else
 case "${COMP_WORDS[1]} ${COMP_WORDS[2]}" in
 'session status') choices='--id --output --lock-wait --verbose ';;
 'session list') choices='--output --lock-wait --verbose ';;
 'session viewport') choices='--id --size --output --lock-wait --verbose ';;
 'tabs list') choices='--id --output --lock-wait --verbose ';;
 'tabs select') choices='--id --target --output --lock-wait --verbose ';;
 'tabs close') choices='--id --target --output --lock-wait --verbose ';;
 'tabs new') choices='--id --url --output --lock-wait --verbose ';;
 'htr status') choices='--id --output --lock-wait --verbose ';;
 'htr takeover') choices='--id --output --lock-wait --verbose ';;
 'htr yield') choices='--id --output --lock-wait --verbose ';;
 'htr share') choices='--id --output --lock-wait --verbose ';;
 'persona list') choices='--output --lock-wait --verbose ';;
 'profile list') choices='--output --lock-wait --verbose ';;
 'profile import') choices='--name --output --lock-wait --verbose ';;
 'profile prune') choices='--min-age --ephemeral --dry-run --reap-orphaned --output --lock-wait --verbose ';;
 'skills list') choices='--output --lock-wait --verbose ';;
 'skills read') choices='--action --output --lock-wait --verbose ';;
 'skills write') choices='--action --output --lock-wait --verbose ';;
 'record start') choices='--id --output --lock-wait --verbose ';;
 'record stop') choices='--id --output --lock-wait --verbose ';;
 'record export') choices='--id --output --lock-wait --verbose ';;
 'muxhost serve') choices='--muxhost-id --runtime-id --browser-session-id --kind --profile-id --profile-dir --chrome-path --debug-port --owner-pid --owner --isolation --mode --goal --idle-ttl --persona --preset --width --height --user-agent --service --account --identity-key --touch --output --lock-wait --verbose ';;
 'muxhost ensure') choices='--muxhost-id --runtime-id --browser-session-id --kind --profile-id --profile-dir --chrome-path --debug-port --owner-pid --owner --isolation --mode --goal --idle-ttl --persona --preset --width --height --user-agent --service --account --identity-key --touch --output --lock-wait --verbose ';;
 'muxhost status') choices='--muxhost-id --runtime-id --browser-session-id --output --lock-wait --verbose ';;
 'muxhost release') choices='--muxhost-id --runtime-id --browser-session-id --output --lock-wait --verbose ';;
 'muxhost shutdown') choices='--muxhost-id --runtime-id --browser-session-id --output --lock-wait --verbose ';;
 'act click') choices='--id --x --y --unit --frame --observation-id --button --count --output --lock-wait --verbose ';;
 'act tap') choices='--id --x --y --unit --frame --observation-id --output --lock-wait --verbose ';;
 'act hover') choices='--id --x --y --unit --frame --observation-id --output --lock-wait --verbose ';;
 'act fill') choices='--id --text --text-file --sensitive --output --lock-wait --verbose ';;
 'act type') choices='--id --text --text-file --focused --output --lock-wait --verbose ';;
 'act press') choices='--id --key --output --lock-wait --verbose ';;
 'act focus') choices='--id --output --lock-wait --verbose ';;
 'act select') choices='--id --value --output --lock-wait --verbose ';;
 'act set-checked') choices='--id --checked --output --lock-wait --verbose ';;
 'act scroll') choices='--id --x --y --unit --frame --observation-id --direction --steps --delta-x --delta-y --output --lock-wait --verbose ';;
 'act scrollto') choices='--id --output --lock-wait --verbose ';;
 'act drag') choices='--id --x --y --unit --frame --observation-id --end-x --end-y --output --lock-wait --verbose ';;
 'act swipe') choices='--id --x --y --unit --frame --observation-id --end-x --end-y --output --lock-wait --verbose ';;
 'act navigate') choices='--id --url --output --lock-wait --verbose ';;
 'act reload') choices='--id --output --lock-wait --verbose ';;
 'act back') choices='--id --output --lock-wait --verbose ';;
 'act forward') choices='--id --output --lock-wait --verbose ';;
 'act zoom') choices='--id --value --output --lock-wait --verbose ';;
 'act keyboard') choices='--id --value --output --lock-wait --verbose ';;
 esac
 if [[ -z $choices ]]; then case "${COMP_WORDS[1]}" in
 'act') choices='click tap hover fill type press focus select set-checked scroll scrollto drag swipe navigate reload back forward zoom keyboard ';;
 'audit') choices='--id --suite --check --threshold --format --output --lock-wait --verbose ';;
 'capabilities') choices='--output --lock-wait --verbose ';;
 'check') choices='--id --observation --assert --file --using --vision-url --vision-provider --vision-model --vision-api-key --output --lock-wait --verbose ';;
 'close') choices='--id --force --output --lock-wait --verbose ';;
 'completion') choices='--output --lock-wait --verbose ';;
 'cookie-import') choices='--id --browser --domain --output --lock-wait --verbose ';;
 'diff') choices='--output --lock-wait --verbose ';;
 'eval') choices='--id --output --lock-wait --verbose ';;
 'export-contract') choices='--directory --output --lock-wait --verbose ';;
 'gc') choices='--older-than --dry-run --all --output --lock-wait --verbose ';;
 'help') choices='--output --lock-wait --verbose ';;
 'htr') choices='status takeover yield share ';;
 'journey') choices='--scenario --mode --persona --engine --safari-device --viewport --profile --evidence --allow-host --browser-chrome --user-agent --goal --stealth --id --file --base-url --fail-fast --output --lock-wait --verbose ';;
 'muxhost') choices='serve ensure status release shutdown ';;
 'observe') choices='--id --screenshot --content --scope --top --budget --health --tree --all --hit-audit --annotate --output --lock-wait --verbose ';;
 'once') choices='--scenario --mode --persona --engine --safari-device --viewport --profile --evidence --allow-host --browser-chrome --user-agent --goal --stealth --screenshot --content --health --tree --all --annotate --output --lock-wait --verbose ';;
 'open') choices='--scenario --mode --persona --engine --safari-device --viewport --profile --evidence --allow-host --browser-chrome --user-agent --goal --stealth --id --output --lock-wait --verbose ';;
 'persona') choices='list ';;
 'profile') choices='list import prune ';;
 'protocol') choices='--output --lock-wait --verbose ';;
 'record') choices='start stop export ';;
 'session') choices='status list viewport ';;
 'skills') choices='list read write ';;
 'tabs') choices='list select close new ';;
 'version') choices='--output --lock-wait --verbose ';;
 'wait') choices='--id --assert --timeout --duration --output --lock-wait --verbose ';;
 esac; fi
 fi
 COMPREPLY=($(compgen -W "$choices" -- "$cur"))
}
complete -F _dw_browser_v2 dw-browser
