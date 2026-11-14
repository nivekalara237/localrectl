package cli

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/nivekalara237/localrectl/config"
)

// cosignCommand is the program --verify-signature runs, looked up in PATH.
const cosignCommand = "cosign"

const cosignInstallURL = "https://docs.sigstore.dev/cosign/system_config/installation/"

// signedChecksums maps each file name to the sha256 the signed checksums file gives.
type signedChecksums map[string]string

// validateSignatureFlags checks the flags of --verify-signature: the signer is given either by
// a public key, --key, or by the identity of its certificate and the issuer of that identity.
func (f *pdFlags) validateSignatureFlags() error {
	hasIdentity := f.certificateIdentity != "" || f.certificateIdentityRegexp != ""
	if !f.verifySignature {
		if f.key != "" || hasIdentity || f.certificateOIDCIssuer != "" {
			return config.Usage(errors.New("--key, --certificate-identity, --certificate-identity-regexp and --certificate-oidc-issuer need --verify-signature"))
		}
		return nil
	}
	if f.noVerify {
		return config.Usage(errors.New("--verify-signature cannot be used with --no-verify"))
	}
	if f.key != "" {
		if hasIdentity || f.certificateOIDCIssuer != "" {
			return config.Usage(errors.New("--key cannot be used with --certificate-identity, --certificate-identity-regexp or --certificate-oidc-issuer"))
		}
		return fileExists("public key", f.key)
	}
	if !hasIdentity || f.certificateOIDCIssuer == "" {
		return config.Usage(errors.New("--verify-signature needs the expected signer: --key, or --certificate-identity (or --certificate-identity-regexp) with --certificate-oidc-issuer"))
	}
	if f.certificateIdentity != "" && f.certificateIdentityRegexp != "" {
		return config.Usage(errors.New("--certificate-identity cannot be used with --certificate-identity-regexp"))
	}
	return nil
}

// verifySignature checks the signature of the checksums file of the release with cosign, then
// checks that the signed checksums file lists each selected file with the sha256 of the
// release.json. It returns the selected files, each with the signed sha256. A dry run checks
// nothing.
func (d *downloaderCmd) verifySignature(ctx context.Context, release *releaseDoc, selected []releaseArtifact) ([]releaseArtifact, error) {
	checksums := release.Checksums
	if checksums == nil || checksums.Name == "" || checksums.Signature == nil || *checksums.Signature == "" {
		return nil, fmt.Errorf("%s %s has no signed checksums file: its files cannot be checked with --verify-signature", d.productName, d.version)
	}
	if d.flags.dryRun {
		_, _ = fmt.Fprintf(d.printer.Err, "%s  signature not verified (dry run)\n", checksums.Name)
		return selected, nil
	}
	cosignPath, err := exec.LookPath(cosignCommand)
	if err != nil {
		return nil, fmt.Errorf("--verify-signature needs cosign, which is not installed: see %s", cosignInstallURL)
	}

	workDir, err := os.MkdirTemp("", "localrectl-signature-*")
	if err != nil {
		return nil, err
	}
	d.tempPaths = append(d.tempPaths, workDir)

	checksumsPath, err := d.fetchAttachment(ctx, checksums.Name, workDir)
	if err != nil {
		return nil, err
	}
	args, err := d.cosignArgs(ctx, checksums, workDir)
	if err != nil {
		return nil, err
	}
	args = append(args, checksumsPath)

	d.log.DebugContext(ctx, "cosign", "path", cosignPath, "args", args)
	output, err := exec.CommandContext(ctx, cosignPath, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("the signature of %s is not valid: %s", checksums.Name, orDefault(strings.TrimSpace(string(output)), err.Error()))
	}
	_, _ = fmt.Fprintf(d.printer.Err, "%s  signature verified (cosign)\n", checksums.Name)

	sums, err := parseChecksums(checksumsPath)
	if err != nil {
		return nil, fmt.Errorf("the signed checksums file %s: %w", checksums.Name, err)
	}
	return checkSigned(selected, sums, checksums.Name)
}

// cosignArgs downloads the signature of the checksums file, and its certificate if any, into
// workDir, and returns the arguments of cosign verify-blob, without the checksums file.
func (d *downloaderCmd) cosignArgs(ctx context.Context, checksums *releaseChecksums, workDir string) ([]string, error) {
	signatureName, err := attachmentName(*checksums.Signature, checksums.Name)
	if err != nil {
		return nil, err
	}
	signaturePath, err := d.fetchAttachment(ctx, signatureName, workDir)
	if err != nil {
		return nil, err
	}

	args := []string{"verify-blob"}
	isBundle := strings.HasSuffix(signatureName, ".sigstore.json") || strings.HasSuffix(signatureName, ".bundle")
	if isBundle {
		args = append(args, "--bundle", signaturePath)
	} else {
		args = append(args, "--signature", signaturePath)
	}

	if d.flags.key != "" {
		keyPath, err := config.ExpandPath(d.flags.key)
		if err != nil {
			return nil, err
		}
		return append(args, "--key", keyPath), nil
	}

	if !isBundle {
		if checksums.Certificate == nil || *checksums.Certificate == "" {
			return nil, fmt.Errorf("the signature of %s has no certificate: pass --key with the public key of the signer", checksums.Name)
		}
		certificateName, err := attachmentName(*checksums.Certificate, checksums.Name)
		if err != nil {
			return nil, err
		}
		certificatePath, err := d.fetchAttachment(ctx, certificateName, workDir)
		if err != nil {
			return nil, err
		}
		args = append(args, "--certificate", certificatePath)
	}
	if d.flags.certificateIdentity != "" {
		args = append(args, "--certificate-identity", d.flags.certificateIdentity)
	} else {
		args = append(args, "--certificate-identity-regexp", d.flags.certificateIdentityRegexp)
	}
	return append(args, "--certificate-oidc-issuer", d.flags.certificateOIDCIssuer), nil
}

// fetchAttachment downloads the file name of the version into workDir, without a progress bar,
// and returns its path.
func (d *downloaderCmd) fetchAttachment(ctx context.Context, name, workDir string) (string, error) {
	if err := safeFileName(name); err != nil {
		return "", err
	}
	progress := newProgressBar(io.Discard, name, false)
	partPath, _, _, err := d.fetchFile(ctx, name, workDir, progress)
	if err != nil {
		return "", err
	}
	target := filepath.Join(workDir, name)
	return target, install(partPath, target, kindChecksums)
}

// attachmentName returns the file name of the attachment at rawURL, a URL of the release.json.
// An attachment covers a file and is named after it: a name that does not start with covered
// is refused.
func attachmentName(rawURL, covered string) (string, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("the release.json holds an invalid attachment URL %q: %w", rawURL, err)
	}
	name, err := url.PathUnescape(path.Base(parsedURL.EscapedPath()))
	if err != nil {
		return "", fmt.Errorf("the release.json holds an invalid attachment URL %q: %w", rawURL, err)
	}
	if name == covered || !strings.HasPrefix(name, covered) || safeFileName(name) != nil {
		return "", fmt.Errorf("the release.json holds an invalid attachment %q for %s", name, covered)
	}
	return name, nil
}

// parseChecksums reads a checksums file of sha256sum: one "<sha256>  <name>" per line, the
// name with a * in binary mode.
func parseChecksums(checksumsPath string) (signedChecksums, error) {
	file, err := os.Open(checksumsPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	sums := signedChecksums{}
	scanner := bufio.NewScanner(io.LimitReader(file, 4<<20))
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: want \"<sha256>  <name>\"", lineNumber)
		}
		checksum, name := strings.ToLower(fields[0]), strings.TrimPrefix(fields[1], "*")
		if decoded, err := hex.DecodeString(checksum); err != nil || len(decoded) != 32 {
			return nil, fmt.Errorf("line %d: %q is not a sha256", lineNumber, fields[0])
		}
		sums[name] = checksum
	}
	return sums, scanner.Err()
}

// checkSigned checks that the signed checksums file lists each selected file, the checksums
// file aside, with the sha256 of the release.json, and gives each file the signed sha256.
func checkSigned(selected []releaseArtifact, sums signedChecksums, checksumsName string) ([]releaseArtifact, error) {
	signed := make([]releaseArtifact, len(selected))
	for index, artifact := range selected {
		if artifact.Kind != kindChecksums {
			checksum, listed := sums[artifact.Name]
			if !listed {
				return nil, fmt.Errorf("%s is not in the signed checksums file %s", artifact.Name, checksumsName)
			}
			if artifact.Sha256 != "" && !strings.EqualFold(artifact.Sha256, checksum) {
				return nil, fmt.Errorf("the release.json and the signed checksums file %s give different sha256 for %s", checksumsName, artifact.Name)
			}
			artifact.Sha256 = checksum
		}
		signed[index] = artifact
	}
	return signed, nil
}
