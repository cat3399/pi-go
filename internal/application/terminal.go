package application

import "github.com/cat3399/pi-go/internal/terminal"

const CommandTerminal CommandType = "terminal"

type TerminalCommand struct{ Request terminal.Request }

func (TerminalCommand) Type() CommandType   { return CommandTerminal }
func (TerminalCommand) applicationCommand() {}

type TerminalResult struct{ Result terminal.Result }

func (TerminalResult) CommandType() CommandType  { return CommandTerminal }
func (TerminalResult) applicationCommandResult() {}
