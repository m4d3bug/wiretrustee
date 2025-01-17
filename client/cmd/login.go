package cmd

import (
	"bufio"
	"context"
	"fmt"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/wiretrustee/wiretrustee/client/internal"
	mgm "github.com/wiretrustee/wiretrustee/management/client"
	mgmProto "github.com/wiretrustee/wiretrustee/management/proto"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"os"
)

var (
	setupKey string

	loginCmd = &cobra.Command{
		Use:   "login",
		Short: "login to the Wiretrustee Management Service (first run)",
		RunE: func(cmd *cobra.Command, args []string) error {
			InitLog(logLevel)

			config, err := internal.GetConfig(managementURL, configPath)
			if err != nil {
				log.Errorf("failed getting config %s %v", configPath, err)
				//os.Exit(ExitSetupFailed)
				return err
			}

			//validate our peer's Wireguard PRIVATE key
			myPrivateKey, err := wgtypes.ParseKey(config.PrivateKey)
			if err != nil {
				log.Errorf("failed parsing Wireguard key %s: [%s]", config.PrivateKey, err.Error())
				//os.Exit(ExitSetupFailed)
				return err
			}

			ctx := context.Background()

			mgmTlsEnabled := false
			if config.ManagementURL.Scheme == "https" {
				mgmTlsEnabled = true
			}

			log.Debugf("connecting to Management Service %s", config.ManagementURL.String())
			mgmClient, err := mgm.NewClient(ctx, config.ManagementURL.Host, myPrivateKey, mgmTlsEnabled)
			if err != nil {
				log.Errorf("failed connecting to Management Service %s %v", config.ManagementURL.String(), err)
				//os.Exit(ExitSetupFailed)
				return err
			}
			log.Debugf("connected to anagement Service %s", config.ManagementURL.String())

			serverKey, err := mgmClient.GetServerPublicKey()
			if err != nil {
				log.Errorf("failed while getting Management Service public key: %v", err)
				//os.Exit(ExitSetupFailed)
				return err
			}

			_, err = loginPeer(*serverKey, mgmClient, setupKey)
			if err != nil {
				log.Errorf("failed logging-in peer on Management Service : %v", err)
				//os.Exit(ExitSetupFailed)
				return err
			}

			err = mgmClient.Close()
			if err != nil {
				log.Errorf("failed closing Management Service client: %v", err)
				//os.Exit(ExitSetupFailed)
				return err
			}

			return nil
		},
	}
)

// loginPeer attempts to login to Management Service. If peer wasn't registered, tries the registration flow.
func loginPeer(serverPublicKey wgtypes.Key, client *mgm.Client, setupKey string) (*mgmProto.LoginResponse, error) {

	loginResp, err := client.Login(serverPublicKey)
	if err != nil {
		if s, ok := status.FromError(err); ok && s.Code() == codes.PermissionDenied {
			log.Debugf("peer registration required")
			return registerPeer(serverPublicKey, client, setupKey)
		} else {
			return nil, err
		}
	}

	log.Info("peer has successfully logged-in to Management Service")

	return loginResp, nil
}

// registerPeer checks whether setupKey was provided via cmd line and if not then it prompts user to enter a key.
// Otherwise tries to register with the provided setupKey via command line.
func registerPeer(serverPublicKey wgtypes.Key, client *mgm.Client, setupKey string) (*mgmProto.LoginResponse, error) {

	var err error
	if setupKey == "" {
		setupKey, err = promptPeerSetupKey()
		if err != nil {
			log.Errorf("failed getting setup key from user: %s", err)
			return nil, err
		}
	}

	validSetupKey, err := uuid.Parse(setupKey)
	if err != nil {
		return nil, err
	}

	log.Debugf("sending peer registration request to Management Service")
	loginResp, err := client.Register(serverPublicKey, validSetupKey.String())
	if err != nil {
		log.Errorf("failed registering peer %v", err)
		return nil, err
	}

	log.Infof("peer has been successfully registered on Management Service")

	return loginResp, nil
}

// promptPeerSetupKey prompts user to enter Setup Key
func promptPeerSetupKey() (string, error) {
	fmt.Print("Enter setup key: ")

	s := bufio.NewScanner(os.Stdin)
	for s.Scan() {
		input := s.Text()
		if input != "" {
			return input, nil
		}
		fmt.Println("Specified key is empty, try again:")

	}

	return "", s.Err()
}

func init() {
	loginCmd.PersistentFlags().StringVar(&setupKey, "setup-key", "", "Setup key obtained from the Management Service Dashboard (used to register peer)")
}
