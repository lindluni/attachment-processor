package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/andygrunwald/go-jira"
	"github.com/google/go-github/v47/github"
	"github.com/thatisuday/commando"
	"golang.org/x/oauth2"
)

type database struct {
	Attachments []*attachment      `json:"attachments"`
	Issues      map[string]*issue  `json:"issues"`
	Tickets     map[string]*ticket `json:"tickets"`
}

type attachment struct {
	Type          string `json:"type"`
	URL           string `json:"url"`
	IssueNumber   int    `json:"issue_number"`
	CommentNumber int64  `json:"comment_number"`
	Path          string `json:"path"`
}

type issue struct {
	URL    string `json:"url"`
	Number int    `json:"number"`
}

type ticket struct {
	Key      string `json:"key"`
	Uploaded bool   `json:"uploaded"`
}

func main() {
	commando.
		SetExecutableName("jira-attachment-processor").
		SetVersion("v1.0.0").
		SetDescription("Utility for migrating GitHub issue attachments to JIRA attachments").
		Register(nil).
		SetAction(func(args map[string]commando.ArgValue, flags map[string]commando.FlagValue) {
			commando.Parse([]string{"help"})
		})

	commando.
		Register("collect").
		SetDescription("Creates the relationships between the attachments, GitHub issues, and JIRA tickets").
		AddFlag("archive", "Path to GitHub repository archive", commando.String, "").
		AddFlag("skip-archive", "Skip expanding the GitHub repository archive", commando.Bool, false).
		AddFlag("github-token", "GitHub personal access token", commando.String, "").
		AddFlag("org", "GitHub organization name", commando.String, "").
		AddFlag("repo", "GitHub repository name", commando.String, "").
		AddFlag("jira-url", "JIRA URL", commando.String, "").
		AddFlag("jira-username", "JIRA username", commando.String, "").
		AddFlag("jira-secret", "JIRA personal access token or password", commando.String, "").
		AddFlag("jira-key", "JIRA project key", commando.String, "").
		SetAction(func(args map[string]commando.ArgValue, flags map[string]commando.FlagValue) {
			err := collect(flags)
			if err != nil {
				fmt.Printf("Failed collecting data: %s\n", err)
			}
		})

	commando.
		Register("upload").
		SetDescription("Uploads attachments to JIRA").
		AddFlag("jira-url", "JIRA URL", commando.String, "").
		AddFlag("jira-username", "JIRA username", commando.String, "").
		AddFlag("jira-secret", "JIRA personal access token or password", commando.String, "").
		SetAction(func(args map[string]commando.ArgValue, flags map[string]commando.FlagValue) {
			err := upload(flags)
			if err != nil {
				fmt.Printf("Failed uploading attachments: %s\n", err)
			}
		})

	commando.
		Register("archive").
		SetDescription("Generates an archive of the exported attachments").
		SetAction(func(args map[string]commando.ArgValue, flags map[string]commando.FlagValue) {
			err := archive()
			if err != nil {
				fmt.Printf("Failed archiving attachments: %s\n", err)
			}
		})

	commando.Parse(nil)
}

func newJIRAClient(username, secret, url string) (*jira.Client, error) {
	tp := jira.BasicAuthTransport{
		Username: username,
		Password: secret,
	}

	return jira.NewClient(tp.Client(), url)
}

func newGitHubClient(token string) *github.Client {
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(ctx, ts)

	return github.NewClient(tc)
}

func expand(path string) error {
	r, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("error opening tarball %s: %s", path, err)
	}

	gzr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)

	for {
		header, err := tr.Next()
		switch {
		case err == io.EOF:
			return nil
		case err != nil:
			return fmt.Errorf("error reading tarball %s: %s", path, err)
		case header == nil:
			continue
		}

		target := filepath.Join("stage", header.Name)
		switch header.Typeflag {

		case tar.TypeDir:
			if _, err := os.Stat(target); err != nil {
				if err := os.MkdirAll(target, 0755); err != nil {
					return fmt.Errorf("failed creating directory %s: %s", target, err)
				}
			}
		case tar.TypeReg:
			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
			if err != nil {
				return fmt.Errorf("failed opening file %s: %s", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				return fmt.Errorf("failed to copy file %s: %s", target, err)
			}
			f.Close()
		}
	}

}

func compress(src string, writers ...io.Writer) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("unable to tar files: %v", err.Error())
	}

	mw := io.MultiWriter(writers...)

	gzw := gzip.NewWriter(mw)
	defer gzw.Close()

	tw := tar.NewWriter(gzw)
	defer tw.Close()

	return filepath.Walk(src, func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		header, err := tar.FileInfoHeader(fi, fi.Name())
		if err != nil {
			return err
		}
		header.Name = strings.TrimPrefix(strings.Replace(file, src, "", -1), string(filepath.Separator))
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		if _, err := io.Copy(tw, f); err != nil {
			return err
		}
		f.Close()

		return nil
	})
}

func processAttachments(db *database) error {
	entries, err := os.ReadDir("stage")
	if err != nil {
		return fmt.Errorf("error reading directory: %s", err)
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "attachments") && strings.HasSuffix(entry.Name(), ".json") {
			path := filepath.Join("stage", entry.Name())
			bytes, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("error reading file %s: %s", path, err)
			}

			var attachments []struct {
				Issue        string `json:"issue"`
				IssueComment string `json:"issue_comment"`
				AssetURL     string `json:"asset_url"`
			}
			if err := json.Unmarshal(bytes, &attachments); err != nil {
				return fmt.Errorf("error unmarshalling JSON from %s: %s", path, err)
			}

			for _, _attachment := range attachments {
				if _attachment.Issue != "" {
					issueTokens := strings.Split(_attachment.Issue, "/")
					issueNumber, err := strconv.ParseInt(issueTokens[len(issueTokens)-1], 10, 64)
					if err != nil {
						return fmt.Errorf("error parsing issue number from %s: %s", _attachment.Issue, err)
					}
					pathTokens := strings.Split(_attachment.AssetURL, "/")
					path := strings.Join(pathTokens[3:], "/")
					entry := &attachment{
						IssueNumber: int(issueNumber),
						Type:        "issue",
						Path:        path,
						URL:         _attachment.Issue,
					}
					db.Attachments = append(db.Attachments, entry)

				} else if _attachment.IssueComment != "" {
					issueTokens := strings.Split(_attachment.IssueComment, "/")
					issueNumber, err := strconv.ParseInt(strings.Split(issueTokens[len(issueTokens)-1], "#")[0], 10, 64)
					if err != nil {
						return fmt.Errorf("error parsing issue number from %s: %s", _attachment.IssueComment, err)
					}
					commentTokens := strings.Split(_attachment.IssueComment, "#")
					commentNumber, err := strconv.ParseInt(strings.Split(commentTokens[len(commentTokens)-1], "issuecomment-")[1], 10, 64)
					if err != nil {
						return fmt.Errorf("error parsing comment number from %s: %s", _attachment.IssueComment, err)
					}
					pathTokens := strings.Split(_attachment.AssetURL, "/")
					path := strings.Join(pathTokens[3:], "/")
					entry := &attachment{
						CommentNumber: commentNumber,
						IssueNumber:   int(issueNumber),
						Type:          "issue_comment",
						Path:          path,
						URL:           _attachment.IssueComment,
					}
					db.Attachments = append(db.Attachments, entry)
				}
			}
		}
	}

	return nil
}

func processIssues(client *github.Client, org, repo string, db *database) error {
	opts := &github.IssueListByRepoOptions{
		State: "all",
		ListOptions: github.ListOptions{
			Page:    1,
			PerPage: 100,
		},
	}
	for {
		issues, resp, err := client.Issues.ListByRepo(context.Background(), org, repo, opts)
		if err != nil {
			if resp.StatusCode == http.StatusNotFound {
				return fmt.Errorf("repository %s/%s not found", org, repo)
			}
			return fmt.Errorf("failed listing issues for %s/%s: %s", org, repo, err)
		}
		fmt.Printf("Processing GitHub issues page %d of %d\n", opts.ListOptions.Page, resp.LastPage)
		for _, _issue := range issues {
			entry := &issue{
				URL:    _issue.GetHTMLURL(),
				Number: _issue.GetNumber(),
			}
			db.Issues[_issue.GetTitle()] = entry
		}
		if resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
		time.Sleep(1 * time.Second)
	}
	return nil
}

func IsEmpty(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	_, err = f.Readdirnames(1)
	if err == io.EOF {
		return true, nil
	}

	return false, err
}

func processTickets(client *jira.Client, key string, db *database) error {
	opts := &jira.SearchOptions{
		StartAt:    0,
		MaxResults: 1000,
	}
	for {
		issues, resp, err := client.Issue.Search(fmt.Sprintf("project=%s", key), opts)
		if err != nil {
			return fmt.Errorf("failed searching for tickets in %s: %s", key, err)
		}
		fmt.Printf("Processing JIRA tickets %d of %d\n", opts.StartAt, resp.Total)
		for _, _issue := range issues {
			entry := &ticket{
				Key:      _issue.Key,
				Uploaded: false,
			}
			db.Tickets[_issue.Fields.Summary] = entry
		}
		if resp.StartAt+resp.MaxResults >= resp.Total {
			break
		}
		opts.StartAt = resp.StartAt + resp.MaxResults
		time.Sleep(1 * time.Second)
	}
	return nil
}

func collect(flags map[string]commando.FlagValue) error {
	archive := flags["archive"].Value.(string)
	skipArchive := flags["skip-archive"].Value.(bool)
	githubToken := flags["github-token"].Value.(string)
	org := flags["org"].Value.(string)
	repo := flags["repo"].Value.(string)
	jiraURL := flags["jira-url"].Value.(string)
	jiraUsername := flags["jira-username"].Value.(string)
	jiraSecret := flags["jira-secret"].Value.(string)
	jiraKey := flags["jira-key"].Value.(string)

	jira, err := newJIRAClient(jiraUsername, jiraSecret, jiraURL)
	if err != nil {
		fmt.Printf("Error creating JIRA client: %s", err)
	}

	gh := newGitHubClient(githubToken)

	if _, err := os.Stat("stage"); os.IsNotExist(err) {
		err = os.MkdirAll("stage", 0755)
		if err != nil {
			return fmt.Errorf("failed creating staging directory: %s", err)
		}
	}

	empty, err := IsEmpty("stage")
	if err != nil {
		return fmt.Errorf("failed checking if staging directory empty: %s", err)
	}

	if !skipArchive {
		if empty {
			fmt.Println("Expanding archive")
			err := expand(archive)
			if err != nil {
				return fmt.Errorf("failed expanding tarball: %s", err)
			}
		} else {
			fmt.Println("Staging directory not empty, skipping archive expansion")
		}
	} else {
		if empty {
			return fmt.Errorf("staging directory is empty, but --skip-archive was specified")
		}
	}

	db := &database{
		Attachments: []*attachment{},
		Issues:      make(map[string]*issue),
		Tickets:     make(map[string]*ticket),
	}

	fmt.Println("Processing GitHub archive")
	err = processAttachments(db)
	if err != nil {
		return fmt.Errorf("failed processing attachments: %s", err)
	}

	fmt.Println("Processing GitHub issues")
	err = processIssues(gh, org, repo, db)
	if err != nil {
		return fmt.Errorf("failed processing issues: %s", err)
	}

	fmt.Println("Processing JIRA tickets")
	err = processTickets(jira, jiraKey, db)
	if err != nil {
		return fmt.Errorf("failed processing tickets: %s", err)
	}

	fmt.Println("Writing database to disk")
	bytes, err := json.Marshal(db)
	if err != nil {
		return fmt.Errorf("failed marshalling database: %s", err)
	}
	err = os.WriteFile("database.json", bytes, 0644)

	return nil
}

func upload(flags map[string]commando.FlagValue) error {
	jiraURL := flags["jira-url"].Value.(string)
	jiraUsername := flags["jira-username"].Value.(string)
	jiraSecret := flags["jira-secret"].Value.(string)

	jira, err := newJIRAClient(jiraUsername, jiraSecret, jiraURL)
	if err != nil {
		log.Panicf("Error creating JIRA client: %s", err)
	}

	bytes, err := os.ReadFile("database.json")
	if err != nil {
		return fmt.Errorf("failed reading database: %s", err)
	}

	db := &database{}
	err = json.Unmarshal(bytes, db)
	if err != nil {
		return fmt.Errorf("failed unmarshalling database: %s", err)
	}

	for title, ticket := range db.Tickets {
		if ticket.Uploaded {
			continue
		}
		issue := db.Issues[title]
		if issue != nil {
			for _, attachment := range db.Attachments {
				if attachment.IssueNumber == issue.Number {
					path := filepath.Join("stage", attachment.Path)
					nameTokens := strings.Split(attachment.Path, "/")
					name := nameTokens[len(nameTokens)-1]
					file, err := os.Open(path)
					if err != nil {
						return fmt.Errorf("failed opening attachment: %s", err)
					}
					fmt.Printf("Uploading attachment %s to %s\n", path, ticket.Key)
					_, resp, err := jira.Issue.PostAttachment(ticket.Key, file, name)
					if err != nil {
						file.Close()
						return fmt.Errorf("failed uploading attachment: %s", err)
					}
					if resp.StatusCode != 200 {
						file.Close()
						return fmt.Errorf("failed uploading attachment: %s", resp.Status)
					}
					file.Close()

					db.Tickets[title].Uploaded = true

					bytes, err := json.Marshal(db)
					if err != nil {
						return fmt.Errorf("failed marshalling database: %s", err)
					}
					err = os.WriteFile("database.json", bytes, 0644)
					if err != nil {
						return fmt.Errorf("failed writing database: %s", err)
					}
				}
			}
		}
	}
	fmt.Println("All attachments uploaded")

	return nil
}

func copy(src, dst string) error {
	sourceFileStat, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("failed getting file stats: %s", err)
	}

	if !sourceFileStat.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}

	source, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed opening source file: %s", err)
	}
	defer source.Close()

	destination, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("failed creating destination file: %s", err)
	}
	defer destination.Close()
	_, err = io.Copy(destination, source)
	if err != nil {
		return fmt.Errorf("failed copying file: %s", err)
	}

	return nil
}

func archive() error {
	if _, err := os.Stat("archive"); os.IsNotExist(err) {
		fmt.Println("Creating archive directory")
		err := os.Mkdir("archive", 0755)
		if err != nil {
			return fmt.Errorf("failed creating archive directory: %s", err)
		}
	} else {
		fmt.Println("Archive directory already exists, deleting contents")
		err := os.RemoveAll("archive")
		if err != nil {
			return fmt.Errorf("failed deleting archive directory: %s", err)
		}
		fmt.Println("Creating new archive directory")
		err = os.Mkdir("archive", 0755)
		if err != nil {
			return fmt.Errorf("failed creating archive directory: %s", err)
		}
	}

	bytes, err := os.ReadFile("database.json")
	if err != nil {
		return fmt.Errorf("failed reading database: %s", err)
	}

	db := &database{}
	err = json.Unmarshal(bytes, db)
	if err != nil {
		return fmt.Errorf("failed unmarshalling database: %s", err)
	}

	fmt.Println("Copying files to archive directory")
	for _, attachment := range db.Attachments {
		nameTokens := strings.Split(attachment.Path, "/")
		name := nameTokens[len(nameTokens)-1]
		if attachment.Type == "issue" {
			srcPath := filepath.Join("stage", attachment.Path)
			dstPath := filepath.Join("archive", fmt.Sprintf("%d_%s", attachment.IssueNumber, name))
			err := copy(srcPath, dstPath)
			if err != nil {
				return fmt.Errorf("failed copying issue attachment: %s", err)
			}
		} else {
			srcPath := filepath.Join("stage", attachment.Path)
			dstPath := filepath.Join("archive", fmt.Sprintf("%d_%d_%s", attachment.IssueNumber, attachment.CommentNumber, name))
			err := copy(srcPath, dstPath)
			if err != nil {
				return fmt.Errorf("failed copying issue comment attachment: %s", err)
			}
		}
	}

	file, err := os.Create("processed_archive.tgz")
	if err != nil {
		return fmt.Errorf("failed opening archive: %s", err)
	}
	defer file.Close()

	fmt.Println("Compressing archive")
	err = compress("archive", file)
	if err != nil {
		return fmt.Errorf("failed compressing archive: %s", err)
	}
	fmt.Println("Archive compressed: processed_archive.tgz")

	return nil
}
