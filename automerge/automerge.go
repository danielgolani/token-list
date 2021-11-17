package main

import (
	"bufio"
	"bytes"
	"context"
	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	cueerrors "cuelang.org/go/cue/errors"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/google/go-github/v40/github"
	"github.com/sourcegraph/go-diff/diff"
	"github.com/tailscale/hujson"
	"golang.org/x/net/context/ctxhttp"
	"golang.org/x/oauth2"
	"io"
	"k8s.io/klog/v2"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

//go:embed schema.cue
var schema []byte

type TokenList struct {
	Name      string          `json:"name"`
	LogoURI   string          `json:"logoURI"`
	Keywords  []string        `json:"keywords"`
	Tags      json.RawMessage `json:"tags"`
	Timestamp string          `json:"timestamp"`
	Tokens    []Token         `json:"tokens"`
	Version   struct {
		Major int `json:"major"`
		Minor int `json:"minor"`
		Patch int `json:"patch"`
	} `json:"version"`
}

type Token struct {
	ChainId    int               `json:"chainId"`
	Address    string            `json:"address"`
	Symbol     string            `json:"symbol"`
	Name       string            `json:"name"`
	Decimals   int               `json:"decimals"`
	LogoURI    string            `json:"logoURI"`
	Tags       []string          `json:"tags,omitempty"`
	Extensions map[string]string `json:"extensions,omitempty"`
}

type Automerger struct {
	client       *github.Client
	owner        string
	repo         string
	cuer         *cue.Context
	cues         cue.Value
	r            *git.Repository
	tl           TokenList
	fs           billy.Filesystem
	knownAddrs   map[string]bool
	knownSymbols map[string]bool
	knownNames   map[string]bool
}

const (
	tokenlistPath = "src/tokens/solana.tokenlist.json"
)

type ErrInvalidSchema error
type ErrManualReviewNeeded error

func loadCueSchema(r *cue.Context, schema []byte, topLevel string) (*cue.Value, error) {
	v := r.CompileBytes(schema)
	if v.Err() != nil {
		return nil, v.Err()
	}
	v = v.LookupPath(cue.MakePath(cue.Def(topLevel)))
	if v.Err() != nil {
		return nil, v.Err()
	}
	return &v, nil
}

func NewAutomerger(owner string, repo string, token string) *Automerger {
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)

	tc := oauth2.NewClient(context.Background(), ts)

	r := cuecontext.New()
	s, err := loadCueSchema(r, schema, "StrictTokenInfo")
	if err != nil {
		panic(err)
	}

	return &Automerger{
		client:       github.NewClient(tc),
		owner:        owner,
		repo:         repo,
		cuer:         r,
		cues:         *s,
		knownAddrs:   map[string]bool{},
		knownSymbols: map[string]bool{},
		knownNames:   map[string]bool{},
	}
}

func (m *Automerger) GetCurrentUser(ctx context.Context) (*github.User, error) {
	acc, _, err := m.client.Users.Get(ctx, "")
	if err != nil {
		return nil, err
	}

	return acc, nil
}

func (m *Automerger) GetOpenPRs(ctx context.Context) ([]*github.PullRequest, error) {
	// paginate through all open PRs
	var allPRs []*github.PullRequest
	opt := &github.PullRequestListOptions{
		State: "open",
	}
	for {
		klog.V(1).Infof("page %d", opt.Page)
		prs, resp, err := m.client.PullRequests.List(ctx, m.owner, m.repo, opt)
		if err != nil {
			return nil, err
		}
		allPRs = append(allPRs, prs...)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}

	return allPRs, nil
}

func (m *Automerger) InitRepo() error {
	pwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current working directory: %v", err)
	}

	var fs billy.Filesystem
	fs = memfs.New()

	r, err := git.Clone(memory.NewStorage(), fs, &git.CloneOptions{
		URL:      "file:///" + pwd,
		Depth:    1,
		Progress: os.Stderr,
	})
	if err != nil {
		return err
	}

	m.r = r
	m.fs = fs
	return nil
}

func (m *Automerger) InitTokenlist() error {
	f, err := m.fs.Open(tokenlistPath)
	if err != nil {
		return fmt.Errorf("failed to open tokenlist: %v", err)
	}
	defer f.Close()

	var tl TokenList
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&tl); err != nil {
		return fmt.Errorf("failed to decode tokenlist: %v", err)
	}

	m.tl = tl

	head, err := m.r.Head()
	if err != nil {
		return fmt.Errorf("failed to get HEAD: %v", err)
	}

	for _, t := range tl.Tokens {
		m.storeKnownToken(&t)
	}

	klog.Infof("current tokenlist loaded from %s (%s) with %d tokens",
		head.Hash(),
		head.Name(),
		len(m.tl.Tokens))

	return nil
}

func (m *Automerger) storeKnownToken(t *Token) {
	m.knownAddrs[t.Address] = true
	m.knownSymbols[t.Symbol] = true
	m.knownNames[t.Name] = true
}

func (m *Automerger) IsKnownToken(t *Token) error {
	if _, ok := m.knownAddrs[t.Address]; ok {
		return fmt.Errorf("token address %s is already used", t.Address)
	}
	if _, ok := m.knownSymbols[t.Symbol]; ok {
		return fmt.Errorf("token symbol %s is already used", t.Symbol)
	}
	if _, ok := m.knownNames[t.Name]; ok {
		return fmt.Errorf("token name %s is already used", t.Name)
	}
	return nil
}

func (m *Automerger) ProcessPR(ctx context.Context, pr *github.PullRequest) error {
	klog.Infof("processing PR %s", pr.GetHTMLURL())

	// Get diff
	d, resp, err := m.client.PullRequests.GetRaw(ctx, m.owner, m.repo, pr.GetNumber(),
		github.RawOptions{Type: github.Diff})
	if err != nil {
		return fmt.Errorf("failed to get diff: %w", err)
	}

	klog.V(1).Infof("rate limit: %v", resp.Rate)

	md, err := diff.ParseMultiFileDiff([]byte(d))
	if err != nil {
		return fmt.Errorf("failed to parse diff: %w", err)
	}

	assets, tl, err := m.parseDiff(md)
	if err != nil {
		if err := m.reportError(pr, err); err != nil {
			return fmt.Errorf("failed to report error: %v", err)
		}
		klog.Warningf("failed to parse diff: %v", err)
		return nil
	}

	tt, err := m.processTokenlist(ctx, tl, assets)
	if err != nil {
		if err := m.reportError(pr, err); err != nil {
			return fmt.Errorf("failed to report error: %v", err)
		}
		klog.Warningf("failed to process tokenlist: %v", err)
		return nil
	}

	if err := m.commitTokenDiff(tt, pr, assets); err != nil {
		if err := m.reportError(pr, err); err != nil {
            return fmt.Errorf("failed to report error: %v", err)
        }
		klog.Warningf("failed to commit token diff: %v", err)
		return nil
	}

	return nil
}

func (m *Automerger) Push(remote string, remoteHead string, force bool) error {
	klog.Infof("pushing to %s/%s (force: %v)", remote, remoteHead, force)

	if force && (remoteHead == "main" || remoteHead == "master") {
		return fmt.Errorf("refusing to force push to main branch")
	}

	// get current HEAD
	head, err := m.r.Head()
	if err != nil {
		return fmt.Errorf("failed to get HEAD: %v", err)
	}

	refspec := fmt.Sprintf("%s:refs/heads/%s", head.Name(), remoteHead)
	klog.V(1).Infof("refspec: %s", refspec)

	if err := m.r.Push(&git.PushOptions{
		RemoteName: remote,
		Progress:   os.Stderr,
		RefSpecs: []config.RefSpec{
			config.RefSpec(refspec),
		},
		Force: force,
	}); err != nil {
		return fmt.Errorf("failed to push: %v", err)
	}

	return nil
}

func (m *Automerger) parseDiff(md []*diff.FileDiff) ([]string, *diff.FileDiff, error) {
	assets := make([]string, 0)
	var tlDiff *diff.FileDiff

	for _, z := range md {
		newFile := strings.TrimPrefix(z.NewName, "b/")
		klog.V(1).Infof("found file: %s", newFile)

		switch {
		case strings.HasPrefix(newFile, "assets/"):
			if z.OrigName != "/dev/null" {
				return nil, nil, fmt.Errorf("found modified asset file %s - only new assets are allowed", newFile)
			}
			p := strings.Split(newFile, "/")
			if len(p) != 4 || p[1] != "mainnet" {
				return nil, nil, fmt.Errorf("invalid asset path: %s", newFile)
			}

			switch path.Ext(p[3]) {
			case ".png", ".jpg", ".svg":
			default:
				return nil, nil, fmt.Errorf("invalid asset extension: %s (wants png, jpg, svg)", newFile)
			}

			assets = append(assets, newFile)
		case newFile == "src/tokens/solana.tokenlist.json":
			if tlDiff != nil {
				return nil, nil, fmt.Errorf("found multiple tokenlist diffs")
			}
			tlDiff = z
			klog.V(1).Infof("found solana.tokenlist.json")
		default:
			// Unknown file modified - fail
			return nil, nil, fmt.Errorf("unsupported file modified: %s", newFile)
		}
	}

	if tlDiff == nil {
		return nil, nil, fmt.Errorf("no tokenlist diff found")
	}

	return assets, tlDiff, nil
}

func (m *Automerger) commitTokenDiff(tt []Token, pr *github.PullRequest, assets []string) error {
	klog.Infof("committing change for %d", pr.GetNumber())

	tl := m.tl
	tl.Tokens = append(m.tl.Tokens, tt...)

	// marshal tl
	b, err := json.MarshalIndent(tl, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal tokenlist: %v", err)
	}

	w, err := m.r.Worktree()
	if err != nil {
		panic(err)
	}

	// request and write out assets to file
	for _, asset := range assets {
		uri := fmt.Sprintf(`https://raw.githubusercontent.com/solana-labs/token-list/%s/%s`, pr.GetHead().GetSHA(), asset)
		klog.V(1).Infof("downloading asset %s", uri)
		resp, err := http.Get(uri)
		if err != nil {
			resp.Body.Close()
            return fmt.Errorf("failed to get asset: %v", err)
        }

		// fail if resp is larger than 200KiB
		if resp.ContentLength > 200*1024 {
            resp.Body.Close()
            return fmt.Errorf("asset too large: %s is %d KiB (must be less than 200 KiB)", asset, resp.ContentLength/1024)
        }

		if err := m.fs.MkdirAll(path.Dir(asset), 0755); err != nil {
			resp.Body.Close()
			return fmt.Errorf("failed to create asset directory: %v", err)
		}
		f, err := m.fs.Create(asset)
		if err != nil {
			resp.Body.Close()
			return fmt.Errorf("failed to create asset file: %v", err)
		}
		if _, err := io.Copy(f, resp.Body); err != nil {
			resp.Body.Close()
            return fmt.Errorf("failed to write asset file: %v", err)
        }
		resp.Body.Close()
		f.Close()
		w.Add(asset)
	}

	// write to file
	f, err := m.fs.Create(tokenlistPath)
	if err != nil {
		return fmt.Errorf("failed to create tokenlist file: %v", err)
	}
	defer f.Close()

	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("failed to write tokenlist file: %v", err)
	}

	title := pr.GetTitle()
	if title == "" {
		title = fmt.Sprintf("Merge #%d", pr.GetNumber())
	}

	_, err = w.Add(tokenlistPath)
	if err != nil {
		return fmt.Errorf("failed to add tokenlist file: %v", err)
	}

	user := pr.GetUser()
	if user == nil || user.Login == nil {
		return fmt.Errorf("failed to get user")
	}

	h, err := w.Commit(
		fmt.Sprintf("%s\n\nCloses #%d", title, pr.GetNumber()),
		&git.CommitOptions{
			Author: &object.Signature{
				Name:  *user.Login,
				Email: fmt.Sprintf("%s@users.noreply.github.com", *user.Login),
				When:  *pr.UpdatedAt,
			},
			Committer: &object.Signature{
				Name:  "token-list automerge",
				Email: "certus-bot@users.noreply.github.com",
				When:  time.Now(),
			},
		})
	if err != nil {
		return fmt.Errorf("failed to commit: %v", err)
	}

	m.tl = tl
	for _, t := range tt {
		m.storeKnownToken(&t)
	}

	klog.V(1).Infof("committed %s (%s)", h, title)

	return nil
}

func (m *Automerger) reportError(pr *github.PullRequest, err error) error {
	return nil
}

func (m *Automerger) processTokenlist(ctx context.Context, d *diff.FileDiff, assets []string) ([]Token, error) {
	assetAddrs := make([]string, len(assets))
	for i, a := range assets {
        assetAddrs[i] = strings.Split(a, "/")[2]
    }

	// log assets
	klog.V(1).Infof("found %d image assets", len(assetAddrs))
	for _, a := range assetAddrs {
		klog.V(1).Infof("  %s", a)
	}

	var res []Token

	for _, h := range d.Hunks {
		body := string(h.Body)
		body = strings.Trim(body, "\n")

		var plain bytes.Buffer

		// Extract added lines
		scanner := bufio.NewScanner(strings.NewReader(body))
		i := 1
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "-") {
				return nil, fmt.Errorf("found removed line: %s", line)
			}
			if strings.HasPrefix(line, "!") {
				return nil, fmt.Errorf("found modified line: %s", line)
			}
			if strings.HasPrefix(line, " ") {
				continue
			}
			if !strings.HasPrefix(line, "+") {
				return nil, fmt.Errorf("unknown diff op: %s", line)
			}
			line = strings.TrimPrefix(line, "+")
			klog.V(2).Infof("ADD: %d: %s", i, line)
			plain.Write([]byte(line + "\n"))
			i++
		}

		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("failed to scan hunks: %w", err)
		}

		// Strip trailing comma
		s := plain.String()
		s = strings.Trim(s, "\n ")
		s = strings.TrimSuffix(s, ",")

		// Strip leading },
		if strings.HasPrefix(s, "},") {
			s = strings.TrimPrefix(s, "},")
			s = s + "\n}"
		}

		// Balance parenthesis
		delta := strings.Count(s, "{") - strings.Count(s, "}")
		for i := 0; i < delta; i++ {
			s = s + "\n}"
		}
		for i := delta; i < 0; i++ {
			s = strings.TrimSuffix(s, "}")
		}

		// Figure out whether to parse multiple objects
		var multi bool
		if strings.Count(s, `"chainId"`) > 1 {
			multi = true
			s = fmt.Sprintf("[\n%s\n]", s)
		}

		// Preprocess using custom JSON parser that ignores trailing commas
		ast, err := hujson.Parse([]byte(s))
		if err != nil {
			return nil, fmt.Errorf("failed to normalize JSON: %v", err)
		}
		ast.Standardize()
		b := ast.Pack()

		var tt []Token
		dec := json.NewDecoder(bytes.NewBuffer(b))
		dec.DisallowUnknownFields()
		if multi {
			if err := dec.Decode(&tt); err != nil {
				return nil, fmt.Errorf("failed to parse JSON: %v", err)
			}
		} else {
			var t Token
			if err := dec.Decode(&t); err != nil {
				return nil, fmt.Errorf("failed to parse JSON: %v", err)
			}
			tt = []Token{t}
		}

		knownSymbols := map[string]bool{}
		knownAddrs := map[string]bool{}
		knownNames := map[string]bool{}
		for _, t := range tt {
			if knownSymbols[t.Symbol] {
				return nil, fmt.Errorf("duplicate symbol within PR")
			}
			if knownAddrs[t.Address] {
				return nil, fmt.Errorf("duplicate address within PR")
			}
			if knownNames[t.Name] {
				return nil, fmt.Errorf("duplicate name within PR")
			}
			knownSymbols[t.Symbol] = true
			knownAddrs[t.Address] = true
			knownNames[t.Name] = true

			v := m.cuer.Encode(t)
			if v.Err() != nil {
				return nil, fmt.Errorf("error encoding to Cue: %v", v.Err())
			}

			u := v.Unify(m.cues)
			if v.Err() != nil {
				return nil, fmt.Errorf("failed to unify with schema: %v", err)
			}

			if err := u.Validate(cue.Final(), cue.Concrete(true)); err != nil {
				// Print last error encountered (which is usually the regex conflict).
				// The full list of errors may be confusing to users who do not understand Cue unification.
				errs := cueerrors.Errors(u.Err())
				last := errs[len(errs)-1]
				return nil, fmt.Errorf("error validating schema: %v", last.Error())
			}

			if strings.Trim(t.Name, " ") == "" {
				return nil, fmt.Errorf("empty token name for %v", t)
			}

			if err := verifyLogoURI(t.LogoURI, assets); err != nil {
				return nil, fmt.Errorf("failed verifying image URI: %v", err)
			}

			if id, ok := t.Extensions["coingeckoId"]; ok {
				if err := verifyCoingeckoId(id); err != nil {
					return nil, fmt.Errorf("failed to verify coingecko ID: %v", err)
				}
			}

			if website, ok := t.Extensions["website"]; ok {
				if err := tryHEADRequest(website); err != nil {
					return nil, fmt.Errorf("failed to verify website: %s", website)
				}
			}

			if twitter, ok := t.Extensions["twitter"]; ok {
				if err := verifyTwitterHandle(twitter); err != nil {
					return nil, fmt.Errorf("failed to verify Twitter handle: %s", twitter)
				}
			}

			klog.V(1).Infof("found valid JSON for token %s", t.Name)
		}

		for _, asset := range assetAddrs {
			var found bool
			for _, t := range tt {
				if t.Address == asset {
					found = true
				}
			}
			if !found {
				return nil, fmt.Errorf("asset file for unknown token found: %s", asset)
			}
		}

		res = append(res, tt...)
	}

	return res, nil
}

func verifyLogoURI(uri string, file []string) error {
	prefix := "https://raw.githubusercontent.com/solana-labs/token-list/main/"
	if strings.HasPrefix(uri, prefix + "assets/") {
		// When a local asset is specified, check if it's part of the PR before checking remotely
		for _, f := range file {
			if uri == (prefix + f) {
				return nil
			}
		}
	}

	klog.V(1).Infof("verifying external image URI %s", uri)

	err2 := tryHEADRequest(uri)
	if err2 != nil {
		return err2
	}

	return nil
}

func tryHEADRequest(uri string) error {
	// Send HEAD request to verify external URIs
	ctx, cancel := context.WithTimeout(context.TODO(), 5*time.Second)
	defer cancel()

	resp, err := ctxhttp.Head(ctx, &http.Client{
		Timeout: 5 * time.Second,
	}, uri)
	if err != nil {
		return fmt.Errorf("failed to verify %s using HEAD request: %v", uri, err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("non-200 response code for URL %s: %d", uri, resp.StatusCode)
	}

	return nil
}

func verifyCoingeckoId(id string) error {
	// TODO: actually test this

	uri := "https://www.coingecko.com/en/coins/" + id

	klog.V(1).Infof("verifying coingeckoId %s", uri)

	ctx, cancel := context.WithTimeout(context.TODO(), 5*time.Second)
	defer cancel()

	resp, err := ctxhttp.Head(ctx, &http.Client{
		Timeout: 5 * time.Second,
	}, uri)
	if err != nil {
		return fmt.Errorf("failed to verify %s using HEAD request: %v", uri, err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("non-200 response code for URL %s: %d", uri, resp.StatusCode)
	}

	return nil

}

type shadowbanResponse struct {
	Profile struct {
		ScreenName string `json:"screen_name"`
		HasTweets  bool   `json:"has_tweets"`
		Exists     bool   `json:"exists"`
	} `json:"profile"`
	Timestamp float64 `json:"timestamp"`
}

func verifyTwitterHandle(uri string) error {
	// use regex to extract the handle
	r := regexp.MustCompile(`^https://twitter.com/(\w+)$`)
	matches := r.FindStringSubmatch(uri)
	if len(matches) != 2 {
        return fmt.Errorf("invalid Twitter URI: %s", uri)
    }
	handle := matches[1]

	klog.V(1).Infof("verifying Twitter handle %s", handle)
	uri = "https://shadowban.eu/.api/" + handle

	ctx, cancel := context.WithTimeout(context.TODO(), 5*time.Second)
	defer cancel()

	resp, err := ctxhttp.Get(ctx, &http.Client{
        Timeout: 5 * time.Second,
    }, uri)

	if err != nil {
		return fmt.Errorf("failed to request %s: %v", uri, err)
	}

	if resp.StatusCode != 200 {
        return fmt.Errorf("non-200 response code for URL %s: %d", uri, resp.StatusCode)
    }

	var sr shadowbanResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return fmt.Errorf("failed to decode response: %v", err)
	}

	if !sr.Profile.Exists {
        return fmt.Errorf("Twitter handle %s does not exist", handle)
    }

	if !sr.Profile.HasTweets {
        return fmt.Errorf("Twitter handle %s has no tweets", handle)
    }

	return nil
}

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		klog.Exitf("GITHUB_TOKEN not set")
	}

	klog.Info("starting automerge")

	m := NewAutomerger("solana-labs", "token-list", token)

	klog.Info("initializing virtual Git worktree")
	if err := m.InitRepo(); err != nil {
		klog.Exitf("failed to initialize virtual Git worktree: %v", err)
	}

	klog.Info("loading tokenlist from worktree")
	if err := m.InitTokenlist(); err != nil {
		klog.Exitf("failed to load tokenlist from worktree: %v", err)
	}

	user, err := m.GetCurrentUser(context.TODO())
	if err != nil {
		klog.Exitf("failed to get current user: %w", err)
	}
	klog.Infof("running as: %s", user.GetLogin())

	r, err := m.GetOpenPRs(context.TODO())
	if err != nil {
		klog.Errorf("error getting open prs: %v", err)
		return
	}

	klog.Infof("processing %d PRs", len(r))

	for _, pr := range r {
		err := m.ProcessPR(context.TODO(), pr)
		if err != nil {
			klog.Exitf("error processing pr: %v", err)
		}
	}

	klog.Info("pushing")
	if err := m.Push("origin", "automerge-pending", true); err != nil {
		klog.Exitf("failed to push: %v", err)
	}

	klog.Info("done")
}
