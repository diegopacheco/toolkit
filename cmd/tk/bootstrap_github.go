package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sigs.k8s.io/yaml"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/google/go-github/v32/github"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1alpha1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1alpha1"
)

var bootstrapGitHubCmd = &cobra.Command{
	Use:   "github",
	Short: "Bootstrap GitHub repository",
	Long: `
The bootstrap command creates the GitHub repository if it doesn't exists and
commits the toolkit components manifests to the master branch.
Then it configure the target cluster to synchronize with the repository.
If the toolkit components are present on the cluster,
the bootstrap command will perform an upgrade if needed.`,
	Example: `  # Create a GitHub personal access token and export it as an env var
  export GITHUB_TOKEN=<my-token>

  # Run bootstrap
  tk bootstrap github --owner=<org or user> --repository=<repo name>
`,
	RunE: bootstrapGitHubCmdRun,
}

var (
	ghOwner      string
	ghRepository string
	ghInterval   time.Duration
)

const ghTokenName = "GITHUB_TOKEN"

func init() {
	bootstrapGitHubCmd.Flags().StringVar(&ghOwner, "owner", "", "GitHub user or organization name")
	bootstrapGitHubCmd.Flags().StringVar(&ghRepository, "repository", "", "GitHub repository name")
	bootstrapGitHubCmd.Flags().DurationVar(&ghInterval, "interval", time.Minute, "sync interval")
	bootstrapCmd.AddCommand(bootstrapGitHubCmd)
}

func bootstrapGitHubCmdRun(cmd *cobra.Command, args []string) error {
	ghToken := os.Getenv(ghTokenName)
	ghURL := fmt.Sprintf("https://github.com/%s/%s", ghOwner, ghRepository)
	if ghToken == "" {
		return fmt.Errorf("%s environment variable not found", ghTokenName)
	}

	if ghOwner == "" || ghRepository == "" {
		return fmt.Errorf("owner and repository are required")
	}

	kubeClient, err := utils.kubeClient(kubeconfig)
	if err != nil {
		return err
	}

	tmpDir, err := ioutil.TempDir("", namespace)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// create GitHub repository if doesn't exists
	logAction("connecting to GitHub")
	if err := initGitHubRepository(ctx, ghOwner, ghRepository, ghToken); err != nil {
		return err
	}

	// clone repository and checkout the master branch
	repo, err := checkoutGitHubRepository(ctx, tmpDir, ghURL, ghToken)
	if err != nil {
		return err
	}
	logSuccess("repository cloned")

	// generate install manifests
	logGenerate("generating manifests")
	manifest, err := generateGitHubInstall(namespace, tmpDir)
	if err != nil {
		return err
	}

	// stage install manifests
	changed, err := commitGitHubManifests(repo, namespace)
	if err != nil {
		return err
	}

	if changed {
		// push install manifests
		if err := pushGitHubRepository(ctx, repo, ghToken); err != nil {
			return err
		}
		logSuccess("components manifests pushed")
	} else {
		logSuccess("components are up to date")
	}

	isInstall := shouldInstallGitHub(ctx, kubeClient, namespace)

	if isInstall {
		// apply install manifests
		logAction("installing components in %s namespace", namespace)
		command := fmt.Sprintf("kubectl apply -f %s", manifest)
		if _, err := utils.execCommand(ctx, ModeOS, command); err != nil {
			return fmt.Errorf("install failed")
		}
		logSuccess("install completed")

		// check rollout status
		logWaiting("verifying installation")
		for _, deployment := range components {
			command = fmt.Sprintf("kubectl -n %s rollout status deployment %s --timeout=%s",
				namespace, deployment, timeout.String())
			if _, err := utils.execCommand(ctx, ModeOS, command); err != nil {
				return fmt.Errorf("install failed")
			} else {
				logSuccess("%s ready", deployment)
			}
		}
	}

	// create or update auth secret
	if err := generateBasicAuth(ctx, namespace, namespace, "git", ghToken); err != nil {
		return err
	}
	logSuccess("authentication configured")

	// generate and push source kustomization
	if isInstall {
		logAction("generating kustomization manifests")
		if err := generateGitHubKustomization(ghURL, namespace, namespace, tmpDir, ghInterval); err != nil {
			return err
		}

		// stage manifests
		changed, err = commitGitHubManifests(repo, namespace)
		if err != nil {
			return err
		}

		// push install manifests
		if changed {
			if err := pushGitHubRepository(ctx, repo, ghToken); err != nil {
				return err
			}
		}
		logSuccess("kustomization manifests pushed")

		logAction("applying kustomization manifests")
		if err := applyGitHubKustomization(ctx, kubeClient, namespace, namespace, tmpDir); err != nil {
			return err
		}
	}

	logSuccess("bootstrap finished")
	return nil
}

func initGitHubRepository(ctx context.Context, owner, name, token string) error {
	isPrivate := true
	isAutoInit := true
	auth := github.BasicAuthTransport{
		Username: "git",
		Password: token,
	}

	client := github.NewClient(auth.Client())
	_, _, err := client.Repositories.Create(ctx, owner, &github.Repository{
		AutoInit: &isAutoInit,
		Name:     &name,
		Private:  &isPrivate,
	})
	if err != nil {
		if !strings.Contains(err.Error(), "name already exists on this account") {
			return fmt.Errorf("github create repository error: %w", err)
		}
	} else {
		logSuccess("repository created")
	}
	return nil
}

func checkoutGitHubRepository(ctx context.Context, path, url, token string) (*git.Repository, error) {
	branch := "master"
	auth := &http.BasicAuth{
		Username: "git",
		Password: token,
	}
	repo, err := git.PlainCloneContext(ctx, path, false, &git.CloneOptions{
		URL:           url,
		Auth:          auth,
		RemoteName:    git.DefaultRemoteName,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		SingleBranch:  true,
		NoCheckout:    false,
		Progress:      nil,
		Tags:          git.NoTags,
	})
	if err != nil {
		return nil, fmt.Errorf("git clone error: %w", err)
	}

	_, err = repo.Head()
	if err != nil {
		return nil, fmt.Errorf("git resolve HEAD error: %w", err)
	}

	return repo, nil
}

func generateGitHubInstall(namespace, tmpDir string) (string, error) {
	kDir := path.Join(tmpDir, ".kustomization")
	defer os.RemoveAll(kDir)

	if err := os.MkdirAll(kDir, os.ModePerm); err != nil {
		return "", fmt.Errorf("generating manifests failed: %w", err)
	}

	if err := genInstallManifests(bootstrapVersion, namespace, components, kDir); err != nil {
		return "", fmt.Errorf("generating manifests failed: %w", err)
	}

	manifestsDir := path.Join(tmpDir, namespace)
	if err := os.MkdirAll(manifestsDir, os.ModePerm); err != nil {
		return "", fmt.Errorf("generating manifests failed: %w", err)
	}

	manifest := path.Join(manifestsDir, "toolkit.yaml")
	if err := buildKustomization(kDir, manifest); err != nil {
		return "", fmt.Errorf("build kustomization failed: %w", err)
	}

	return manifest, nil
}

func commitGitHubManifests(repo *git.Repository, namespace string) (bool, error) {
	w, err := repo.Worktree()
	if err != nil {
		return false, err
	}

	_, err = w.Add(namespace)
	if err != nil {
		return false, err
	}

	status, err := w.Status()
	if err != nil {
		return false, err
	}

	if !status.IsClean() {
		if _, err := w.Commit("Add manifests", &git.CommitOptions{
			Author: &object.Signature{
				Name:  "tk",
				Email: "tk@@users.noreply.github.com",
				When:  time.Now(),
			},
		}); err != nil {
			return false, err
		}
		return true, nil
	}

	return false, nil
}

func pushGitHubRepository(ctx context.Context, repo *git.Repository, token string) error {
	auth := &http.BasicAuth{
		Username: "git",
		Password: token,
	}
	err := repo.PushContext(ctx, &git.PushOptions{
		Auth:     auth,
		Progress: nil,
	})
	if err != nil {
		return fmt.Errorf("git push error: %w", err)
	}
	return nil
}

func generateGitHubKustomization(url, name, namespace, tmpDir string, interval time.Duration) error {
	gvk := sourcev1.GroupVersion.WithKind("GitRepository")
	gitRepository := sourcev1.GitRepository{
		TypeMeta: metav1.TypeMeta{
			Kind:       gvk.Kind,
			APIVersion: gvk.GroupVersion().String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: sourcev1.GitRepositorySpec{
			URL: url,
			Interval: metav1.Duration{
				Duration: interval,
			},
			Reference: &sourcev1.GitRepositoryRef{
				Branch: "master",
			},
			SecretRef: &corev1.LocalObjectReference{
				Name: name,
			},
		},
	}

	gitData, err := yaml.Marshal(gitRepository)
	if err != nil {
		return err
	}

	if err := utils.writeFile(string(gitData), filepath.Join(tmpDir, namespace, "toolkit-source.yaml")); err != nil {
		return err
	}

	gvk = kustomizev1.GroupVersion.WithKind("Kustomization")
	emptyAPIGroup := ""
	kustomization := kustomizev1.Kustomization{
		TypeMeta: metav1.TypeMeta{
			Kind:       gvk.Kind,
			APIVersion: gvk.GroupVersion().String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: kustomizev1.KustomizationSpec{
			Interval: metav1.Duration{
				Duration: 10 * time.Minute,
			},
			Path:  "./",
			Prune: true,
			SourceRef: corev1.TypedLocalObjectReference{
				APIGroup: &emptyAPIGroup,
				Kind:     "GitRepository",
				Name:     name,
			},
		},
	}

	ksData, err := yaml.Marshal(kustomization)
	if err != nil {
		return err
	}

	if err := utils.writeFile(string(ksData), filepath.Join(tmpDir, namespace, "toolkit-kustomization.yaml")); err != nil {
		return err
	}

	return nil
}

func applyGitHubKustomization(ctx context.Context, kubeClient client.Client, name, namespace, tmpDir string) error {
	command := fmt.Sprintf("kubectl apply -f %s", filepath.Join(tmpDir, namespace))
	if _, err := utils.execCommand(ctx, ModeStderrOS, command); err != nil {
		return err
	}

	logWaiting("waiting for kustomization sync")
	if err := wait.PollImmediate(pollInterval, timeout,
		isKustomizationReady(ctx, kubeClient, name, namespace)); err != nil {
		return err
	}

	return nil
}

func shouldInstallGitHub(ctx context.Context, kubeClient client.Client, namespace string) bool {
	namespacedName := types.NamespacedName{
		Namespace: namespace,
		Name:      namespace,
	}
	var kustomization kustomizev1.Kustomization
	if err := kubeClient.Get(ctx, namespacedName, &kustomization); err != nil {
		return true
	}

	return kustomization.Status.LastAppliedRevision == ""
}
