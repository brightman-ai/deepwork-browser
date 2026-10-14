# dw-browser shell completions

Both shell scripts are generated from the executable command declarations. They cover current commands, subcommands and flags; `help COMMAND` and `capabilities` expose exact required flags and enums.

Regenerate with the same candidate that supplies the tool protocol:

```sh
/path/to/dw-browser export-contract --directory guide/runtime-v2/generated
cp guide/runtime-v2/generated/completion.zsh completions/_dw-browser
cp guide/runtime-v2/generated/completion.bash completions/dw-browser.bash
```

For zsh, copy `_dw-browser` to a directory on `fpath` and initialize `compinit`. For bash, source `completions/dw-browser.bash`. Repository generation does not install either script globally.
