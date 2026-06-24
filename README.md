# meowcaller
[![Go Reference](https://pkg.go.dev/badge/github.com/purpshell/meowcaller.svg)](https://pkg.go.dev/github.com/purpshell/meowcaller)

meowcaller is a Go library for the WhatsApp Web VoIP stack. It is 100% pure GO without any CGO dependencies and little to no dependencies of its own. It includes the proprietary audio codec MLOW written and validated completely in GO.

## Discussion
Matrix room: [#meowcaller:matrix.com](https://matrix.to/#/#meowcaller:matrix.com).

Discord channel: #meowcaller in the [WhiskeySockets Discord server](https://whiskey.so/discord).

You can find the underlying spec in the [WhatsApp Calls Research Group](https://wacrg.org). We are under process of standardizing the spec and moving away from whatsapp-rust source of truth comments.

## Usage
The [godoc](https://pkg.go.dev/github.com/purpshell/meowcaller) includes docs for all methods.

There's a range of examples in the [examples](/examples/) directory.

## Features

Core VoIP features are present:

- Outbound calls
- Inbound calls
- Audio calling

Things that are not yet implemented:

- Opus codec fallback for clients not using MLOW (in progress; testing edge cases)
- Group calls (WIP)
- Video calls (WIP)
- Call signalling features (raise hand, lobby, reactions)

## Sponsoring and contribution
You may contribute to the maintenance of this library by sponsoring its maintainers on [GitHub](https://purpshell.dev/sponsor).

You may also submit pull requests and issues where relevant, given you follow the contributor [Code of Conduct](CODE_OF_CONDUCT.md).

## License

This repository follows the MIT license, as stated in the [LICENSE](/LICENSE) file

