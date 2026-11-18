// paperplane-plugin-nodejs 是 nodejs 插件的独立可执行入口。
package main

import (
	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
	nodejs "github.com/lemon-paperplane/lemon-paperplane-plugin-nodejs/plugin"
)

func main() { sdk.Serve(nodejs.New()) }
