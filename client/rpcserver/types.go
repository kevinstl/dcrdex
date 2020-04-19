// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package rpcserver

import (
	"errors"
	"fmt"
	"io/ioutil"
	"strconv"

	"decred.org/dcrdex/client/core"
)

var (
	// ErrArgs is wrapped when arguments to the known command cannot be parsed.
	ErrArgs = errors.New("unable to parse arguments")
	// ErrUnknownCmd is wrapped when the command is not know.
	ErrUnknownCmd = errors.New("unknown command")
)

// versionResponse holds a semver version JSON object.
type versionResponse struct {
	Major uint32 `json:"major"`
	Minor uint32 `json:"minor"`
	Patch uint32 `json:"patch"`
}

// String satisfies the Stringer interface.
func (vr versionResponse) String() string {
	return fmt.Sprintf("%d.%d.%d", vr.Major, vr.Minor, vr.Patch)
}

// preRegisterResponse is used when responding to the preregister route.
type preRegisterResponse struct {
	Fee uint64 `json:"fee"`
}

// openWalletForm is information necessary to open a wallet.
type openWalletForm struct {
	AssetID uint32 `json:"assetID"`
	AppPass string `json:"appPass"`
}

// newWalletForm is information necessary to create a new wallet.
type newWalletForm struct {
	AssetID    uint32 `json:"assetID"`
	Account    string `json:"account"`
	INIPath    string `json:"inipath"`
	WalletPass string `json:"walletPass"`
	AppPass    string `json:"appPass"`
}

// ParseCmdArgs parses arguments to commands for rpcserver requests.
func ParseCmdArgs(cmd string, args []string) (interface{}, error) {
	nArg, exists := nArgs[cmd]
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrUnknownCmd, cmd)
	}
	if err := checkNArgs(len(args), nArg); err != nil {
		return nil, err
	}
	return parsers[cmd](args)
}

// nArgs is a map of routes to the number of arguments accepted. One integer
// indicates an exact match, two are the min and max.
var nArgs = map[string][]int{
	helpRoute:        {0, 1},
	initRoute:        {1},
	versionRoute:     {0},
	preRegisterRoute: {1, 2},
	newWalletRoute:   {5},
	openWalletRoute:  {2},
	closeWalletRoute: {1},
	walletsRoute:     {0},
	registerRoute:    {3, 4},
}

// parsers is a map of commands to parsing functions.
var parsers = map[string](func([]string) (interface{}, error)){
	helpRoute:        parseHelpArgs,
	initRoute:        func(args []string) (interface{}, error) { return args[0], nil },
	versionRoute:     func([]string) (interface{}, error) { return nil, nil },
	preRegisterRoute: parsePreRegisterArgs,
	newWalletRoute:   parseNewWalletArgs,
	openWalletRoute:  parseOpenWalletArgs,
	closeWalletRoute: func(args []string) (interface{}, error) {
		return checkIntArg(args[0], "assetID")
	},
	walletsRoute:  func([]string) (interface{}, error) { return nil, nil },
	registerRoute: parseRegisterArgs,
}

func checkNArgs(have int, want []int) error {
	if len(want) == 1 {
		if want[0] != have {
			return fmt.Errorf("%w: wanted %d but got %d", ErrArgs, want[0], have)
		}
	} else {
		if have < want[0] || have > want[1] {
			return fmt.Errorf("%w: wanted between %d and %d but got %d", ErrArgs, want[0], want[1], have)
		}
	}
	return nil
}

func checkIntArg(arg, name string) (int, error) {
	i, err := strconv.Atoi(arg)
	if err != nil {
		return i, fmt.Errorf("%w, %s must be an integer: %v", ErrArgs, name, err)
	}
	return i, nil
}

func parseHelpArgs(args []string) (interface{}, error) {
	if len(args) == 0 {
		return nil, nil
	}
	return args[0], nil
}

func parseNewWalletArgs(args []string) (interface{}, error) {
	assetID, err := checkIntArg(args[2], "assetID")
	if err != nil {
		return nil, err
	}
	req := &newWalletForm{
		AppPass:    args[0],
		WalletPass: args[1],
		AssetID:    uint32(assetID),
		Account:    args[3],
		INIPath:    args[4],
	}
	return req, nil
}

func parseOpenWalletArgs(args []string) (interface{}, error) {
	assetID, err := checkIntArg(args[1], "assetID")
	if err != nil {
		return nil, err
	}
	req := &openWalletForm{AppPass: args[0], AssetID: uint32(assetID)}
	return req, nil
}

func parseRegisterArgs(args []string) (interface{}, error) {
	fee, err := checkIntArg(args[2], "fee")
	if err != nil {
		return nil, err
	}
	cert := ""
	if len(args) == 4 {
		certB, err := ioutil.ReadFile(args[3])
		if err != nil {
			return nil, fmt.Errorf("error reading %s: %v", args[1], err)
		}
		cert = string(certB)
	}
	req := &core.Registration{
		Password: args[0],
		URL:      args[1],
		Fee:      uint64(fee),
		Cert:     cert,
	}
	return req, nil
}

func parsePreRegisterArgs(args []string) (interface{}, error) {
	cert := ""
	if len(args) > 1 {
		certB, err := ioutil.ReadFile(args[1])
		if err != nil {
			return nil, fmt.Errorf("error reading %s: %v", args[1], err)
		}
		cert = string(certB)
	}
	return &core.PreRegisterForm{
		URL:  args[0],
		Cert: cert,
	}, nil
}
