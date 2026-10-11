package core

import (
	"os"
	"path/filepath"
)

// CloudCredentialArgs are the `docker run` arguments that hand a container
// the invoking person's own cloud credentials for target kind (aws-ecs |
// gcp-cloudrun): the variables passed through BY NAME (never their values on
// a command line), the CLI configuration directories mounted read-only under
// home (the container's $HOME, or where a caller copies them from).
func CloudCredentialArgs(kind, home string) []string {
	var args []string
	user, _ := os.UserHomeDir()
	mount := func(src, dst string) {
		if st, err := os.Stat(src); err == nil && st.IsDir() {
			args = append(args, "-v", src+":"+dst+":ro")
		}
	}
	switch kind {
	case "aws-ecs":
		for _, v := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION"} {
			if os.Getenv(v) != "" {
				args = append(args, "-e", v)
			}
		}
		mount(filepath.Join(user, ".aws"), home+"/.aws")
	case "gcp-cloudrun":
		if f := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); f != "" {
			args = append(args, "-v", f+":/run/gcp-credentials.json:ro", "-e", "GOOGLE_APPLICATION_CREDENTIALS=/run/gcp-credentials.json")
		}
		mount(filepath.Join(user, ".config", "gcloud"), home+"/.config/gcloud")
	}
	return args
}
