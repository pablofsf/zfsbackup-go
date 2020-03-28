// Copyright © 2016 Prateek Malhotra (someone1@gmail.com)
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cmd

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	humanize "github.com/dustin/go-humanize"
	"github.com/juju/ratelimit"
	"github.com/op/go-logging"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh/terminal"

	"github.com/someone1/zfsbackup-go/helpers"
)

var (
	numCores          int
	logLevel          string
	secretKeyRingPath string
	publicKeyRingPath string
	workingDirectory  string
	errInvalidInput   = errors.New("invalid input")
)

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:   "zfsbackup",
	Short: "zfsbackup is a tool used to do off-site backups of ZFS volumes.",
	Long: `zfsbackup is a tool used to do off-site backups of ZFS volumes.
It leverages the built-in snapshot capabilities of ZFS in order to export ZFS
volumes for long-term storage.

zfsbackup uses the "zfs send" command to export, and optionally compress, sign,
encrypt, and split the send stream to files that are then transferred to a
destination of your choosing.`,
	PersistentPreRunE: processFlags,
	PersistentPostRun: postRunCleanup,
	SilenceErrors:     true,
}

// Execute adds all child commands to the root command sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := RootCmd.Execute(); err != nil {
		os.Exit(-1)
	}
}

func init() {
	RootCmd.PersistentFlags().IntVar(&numCores, "numCores", 2, "number of CPU cores to utilize. Do not exceed the number of CPU cores on the system.")
	RootCmd.PersistentFlags().StringVar(&logLevel, "logLevel", "notice", "this controls the verbosity level of logging. Possible values are critical, error, warning, notice, info, debug.")
	RootCmd.PersistentFlags().StringVar(&secretKeyRingPath, "secretKeyRingPath", "", "the path to the PGP secret key ring")
	RootCmd.PersistentFlags().StringVar(&publicKeyRingPath, "publicKeyRingPath", "", "the path to the PGP public key ring")
	RootCmd.PersistentFlags().StringVar(&workingDirectory, "workingDirectory", "~/.zfsbackup", "the working directory path for zfsbackup.")
	RootCmd.PersistentFlags().StringVar(&jobInfo.ManifestPrefix, "manifestPrefix", "manifests", "the prefix to use for all manifest files.")
	RootCmd.PersistentFlags().StringVar(&jobInfo.EncryptMail, "encryptMail", "", "the email of the user used for encryption/decryption from the corresponding public/private keyring.")
	RootCmd.PersistentFlags().StringVar(&jobInfo.SignMail, "signMail", "", "the email of the user used for signing/verifying from the corresponding private/public keyring.")
	RootCmd.PersistentFlags().StringVar(&helpers.ZFSPath, "zfsPath", "zfs", "the path to the zfs executable.")
	RootCmd.PersistentFlags().BoolVar(&helpers.JSONOutput, "jsonOutput", false, "dump results as a JSON string - on success only")
	passphrase = []byte(os.Getenv("PGP_PASSPHRASE"))
}

func resetRootFlags() {
	jobInfo = helpers.JobInfo{}
	numCores = 2
	logLevel = "notice"
	secretKeyRingPath = ""
	publicKeyRingPath = ""
	workingDirectory = "~/.zfsbackup"
	jobInfo.ManifestPrefix = "manifests"
	jobInfo.EncryptMail = ""
	jobInfo.SignMail = ""
	helpers.ZFSPath = "zfs"
	helpers.JSONOutput = false
}

func processFlags(cmd *cobra.Command, args []string) error {
	switch strings.ToLower(logLevel) {
	case "critical":
		logging.SetLevel(logging.CRITICAL, helpers.LogModuleName)
	case "error":
		logging.SetLevel(logging.ERROR, helpers.LogModuleName)
	case "warning", "warn":
		logging.SetLevel(logging.WARNING, helpers.LogModuleName)
	case "notice":
		logging.SetLevel(logging.NOTICE, helpers.LogModuleName)
	case "info":
		logging.SetLevel(logging.INFO, helpers.LogModuleName)
	case "debug":
		logging.SetLevel(logging.DEBUG, helpers.LogModuleName)
	default:
		helpers.AppLogger.Errorf("Invalid log level provided. Was given %s", logLevel)
		return errInvalidInput
	}

	if numCores <= 0 {
		helpers.AppLogger.Errorf("The number of cores to use provided is an invalid value. It must be greater than 0. %d was given.", numCores)
		return errInvalidInput
	}

	if numCores > runtime.NumCPU() {
		helpers.AppLogger.Warningf("Ignoring user provided number of cores (%d) and using the number of detected cores (%d).", numCores, runtime.NumCPU())
		numCores = runtime.NumCPU()
	}
	helpers.AppLogger.Infof("Setting number of cores to: %d", numCores)
	runtime.GOMAXPROCS(numCores)

	if secretKeyRingPath != "" {
		if err := helpers.LoadPrivateRing(secretKeyRingPath); err != nil {
			helpers.AppLogger.Errorf("Could not load private keyring due to an error - %v", err)
			return errInvalidInput
		}
	}
	helpers.AppLogger.Infof("Loaded private key ring %s", secretKeyRingPath)

	if publicKeyRingPath != "" {
		if err := helpers.LoadPublicRing(publicKeyRingPath); err != nil {
			helpers.AppLogger.Errorf("Could not load public keyring due to an error - %v", err)
			return errInvalidInput
		}
	}
	helpers.AppLogger.Infof("Loaded public key ring %s", publicKeyRingPath)

	if err := setupGlobalVars(); err != nil {
		return err
	}
	helpers.AppLogger.Infof("Setting working directory to %s", workingDirectory)
	helpers.PrintPGPDebugInformation()
	return nil
}

func postRunCleanup(cmd *cobra.Command, args []string) {
	err := os.RemoveAll(helpers.BackupTempdir)
	if err != nil {
		helpers.AppLogger.Errorf("Could not clean working temporary directory - %v", err)
	}
}

func setupGlobalVars() error {
	// Setup Tempdir

	if strings.HasPrefix(workingDirectory, "~") {
		usr, err := user.Current()
		if err != nil {
			helpers.AppLogger.Errorf("Could not get current user due to error - %v", err)
			return err
		}
		workingDirectory = filepath.Join(usr.HomeDir, strings.TrimPrefix(workingDirectory, "~"))
	}

	if dir, serr := os.Stat(workingDirectory); serr == nil && !dir.IsDir() {
		helpers.AppLogger.Errorf("Cannot create working directory because another non-directory object already exists in that path (%s)", workingDirectory)
		return errInvalidInput
	} else if serr != nil {
		err := os.Mkdir(workingDirectory, 0755)
		if err != nil {
			helpers.AppLogger.Errorf("Could not create working directory %s due to error - %v", workingDirectory, err)
			return err
		}
	}

	dirPath := filepath.Join(workingDirectory, "temp")
	if dir, serr := os.Stat(dirPath); serr == nil && !dir.IsDir() {
		helpers.AppLogger.Errorf("Cannot create temp dir in working directory because another non-directory object already exists in that path (%s)", dirPath)
		return errInvalidInput
	} else if serr != nil {
		err := os.Mkdir(dirPath, 0755)
		if err != nil {
			helpers.AppLogger.Errorf("Could not create temp directory %s due to error - %v", dirPath, err)
			return err
		}
	}

	tempdir, err := ioutil.TempDir(dirPath, helpers.LogModuleName)
	if err != nil {
		helpers.AppLogger.Errorf("Could not create temp directory due to error - %v", err)
		return err
	}

	helpers.BackupTempdir = tempdir
	helpers.WorkingDir = workingDirectory

	dirPath = filepath.Join(workingDirectory, "cache")
	if dir, serr := os.Stat(dirPath); serr == nil && !dir.IsDir() {
		helpers.AppLogger.Errorf("Cannot create cache dir in working directory because another non-directory object already exists in that path (%s)", dirPath)
		return errInvalidInput
	} else if serr != nil {
		err := os.Mkdir(dirPath, 0755)
		if err != nil {
			helpers.AppLogger.Errorf("Could not create cache directory %s due to error - %v", dirPath, err)
			return err
		}
	}

	if maxUploadSpeed != 0 {
		helpers.AppLogger.Infof("Limiting the upload speed to %s/s.", humanize.Bytes(maxUploadSpeed*humanize.KByte))
		helpers.BackupUploadBucket = ratelimit.NewBucketWithRate(float64(maxUploadSpeed*humanize.KByte), int64(maxUploadSpeed*humanize.KByte))
	}
	return nil
}

func validatePassphrase() {
	var err error
	if len(passphrase) == 0 {
		fmt.Fprint(helpers.Stdout, "Enter passphrase to decrypt encryption key: ")
		passphrase, err = terminal.ReadPassword(0)
		if err != nil {
			helpers.AppLogger.Errorf("Error reading user input for encryption key passphrase: %v", err)
			panic(err)
		}
	}
}

func decryptSignKey() error {
	if jobInfo.SignKey.PrivateKey != nil && jobInfo.SignKey.PrivateKey.Encrypted {
		validatePassphrase()
		if err := jobInfo.SignKey.PrivateKey.Decrypt(passphrase); err != nil {
			helpers.AppLogger.Errorf("Error decrypting private key: %v", err)
			return errInvalidInput
		}
	}

	for _, subkey := range jobInfo.SignKey.Subkeys {
		if subkey.PrivateKey != nil && subkey.PrivateKey.Encrypted {
			validatePassphrase()
			if err := subkey.PrivateKey.Decrypt(passphrase); err != nil {
				helpers.AppLogger.Errorf("Error decrypting subkey's private key: %v", err)
				return errInvalidInput
			}
		}
	}

	return nil
}

func decryptEncryptKey() error {
	if jobInfo.EncryptKey.PrivateKey != nil && jobInfo.EncryptKey.PrivateKey.Encrypted {
		validatePassphrase()
		if err := jobInfo.EncryptKey.PrivateKey.Decrypt(passphrase); err != nil {
			helpers.AppLogger.Errorf("Error decrypting private key: %v", err)
			return errInvalidInput
		}
	}

	for _, subkey := range jobInfo.EncryptKey.Subkeys {
		if subkey.PrivateKey != nil && subkey.PrivateKey.Encrypted {
			validatePassphrase()
			if err := subkey.PrivateKey.Decrypt(passphrase); err != nil {
				helpers.AppLogger.Errorf("Error decrypting subkey's private key: %v", err)
				return errInvalidInput
			}
		}
	}

	return nil
}
