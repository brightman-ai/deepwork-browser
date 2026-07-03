# dw-browser shell completions

## zsh (`_dw-browser`)
子命令 + flag + enum 值补全。重点:新增安全 flag 的值补全 `--remote-writes {deny|confirm|allow}`、`--mode`、`--kind`、`--using`、`--engine`。

安装:
```sh
mkdir -p ~/.zsh/completions
cp completions/_dw-browser ~/.zsh/completions/
# ~/.zshrc 或 ~/.zshrc.local:
fpath=(~/.zsh/completions $fpath)
autoload -Uz compinit && compinit
```
