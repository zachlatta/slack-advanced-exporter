package cmd

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

var (
	emailsApiToken  string
	usersPageLimit  int
	usersPace1RPS   bool
)

var fetchEmailsCmd = &cobra.Command{
	Use:   "fetch-emails",
	Short: "Fetch all file attachments and add them to the output archive",
	RunE:  fetchEmails,
}

func init() {
	fetchEmailsCmd.PersistentFlags().StringVar(&emailsApiToken, "api-token", "", "Slack API token. Can be obtained here: https://api.slack.com/docs/oauth-test-tokens")
	fetchEmailsCmd.PersistentFlags().IntVar(&usersPageLimit, "users-limit", 999, "users.list page size (recommended 200, max 999)")
	fetchEmailsCmd.PersistentFlags().BoolVar(&usersPace1RPS, "users-pace-1rps", true, "pace users.list requests to ~1 request/second to avoid rate limits")
	fetchEmailsCmd.MarkPersistentFlagRequired("api-token")
}

func fetchEmails(cmd *cobra.Command, args []string) error {
	// Open the input archive.
	r, err := zip.OpenReader(inputArchive)
	if err != nil {
		fmt.Printf("Could not open input archive for reading: %s\n", inputArchive)
		os.Exit(1)
	}
	defer r.Close()

	// Open the output archive.
	f, err := os.Create(outputArchive)
	if err != nil {
		fmt.Printf("Could not open the output archive for writing: %s\n\n%s", outputArchive, err)
		os.Exit(1)
	}
	defer f.Close()

	// Create a zip writer on the output archive.
	w := zip.NewWriter(f)

	// Run through all the files in the input archive.
	for _, file := range r.File {
		verbosePrintln(fmt.Sprintf("Processing file: %s\n", file.Name))

		// Open the file from the input archive.
		inReader, err := file.Open()
		if err != nil {
			fmt.Printf("Failed to open file in input archive: %s\n\n%s", file.Name, err)
			os.Exit(1)
		}

		// Copy, because CreateHeader modifies it.
		header := file.FileHeader

		outFile, err := w.CreateHeader(&header)
		if err != nil {
			fmt.Printf("Failed to create file in output archive: %s\n\n%s", file.Name, err)
			os.Exit(1)
		}

		if file.Name == "users.json" {
			err = processUsersJson(outFile, inReader, emailsApiToken)
			if err != nil {
				fmt.Printf("Failed to fetch users' emails.\n\n%s", err)
				os.Exit(1)
			}
		} else {
			_, err = io.Copy(outFile, inReader)
			if err != nil {
				fmt.Printf("Failed to copy file to output archive: %s\n\n%s", file.Name, err)
				os.Exit(1)
			}
		}
	}

	// Close the output zip writer.
	err = w.Close()
	if err != nil {
		fmt.Printf("Failed to close the output archive.\n\n%s", err)
	}

	return nil
}

func processUsersJson(output io.Writer, input io.Reader, slackApiToken string) error {
	verbosePrintln("Found users.json file.")

	// We want to preserve all existing fields in JSON.
	// By using interface{} (instead of struct), we can avoid describing all
	// the fields (new ones might be added by Slack devs in the future!) at the cost of
	// slight inconvenience of type assertions and working with maps.
	var data []map[string]interface{}
	err := json.NewDecoder(input).Decode(&data)
	if err != nil {
		return err
	}

	emails, err := fetchUserEmails(slackApiToken)
	if err != nil {
		return err
	}

	if len(data) == 0 {
		return errors.New("Failed to find any users in users.json. Looks like something went wrong.")
	}

	verbosePrintln("Updating users.json contents with fetched emails.")

	for _, user := range data {
		// These 'ok's only check for type assertion success.
		// Map access would return untyped nil,
		// which is fine, as untyped nil would fail both these type assertions.
		name, _ := user["name"].(string)

		if userid, ok := user["id"].(string); ok {
			if profile, ok := user["profile"].(map[string]interface{}); ok {
				email := emails[userid]

				profile["email"] = email
				log.Printf("%q (%q) -> %q", name, userid, email)
			} else {
				log.Printf("User %q doesn't have 'profile' in JSON file (unexpected error!)", userid)
			}
		} else {
			log.Print("Some user array entry doesn't have id, skipping")
		}
	}
	enc := json.NewEncoder(output)
	// The same indent level as export zip uses.
	enc.SetIndent("", "    ")
	return enc.Encode(&data)
}

func fetchUserEmails(token string) (map[string]string, error) {
	verbosePrintln("Fetching emails from Slack API")

	// Bound and log page size selection
	if usersPageLimit <= 0 {
		usersPageLimit = 200
	}
	if usersPageLimit > 999 {
		usersPageLimit = 999
	}
	if usersPageLimit > 200 {
		verboseLogf("users.list limit set to %d (>200). Slack recommends 200, max allowed is under 1000.", usersPageLimit)
	} else {
		verboseLogf("users.list limit set to %d.", usersPageLimit)
	}

	client := &http.Client{}

	res := make(map[string]string)
	total := 0
	page := 0
	cursor := ""

	for {
		v := url.Values{}
		v.Set("limit", strconv.Itoa(usersPageLimit))
		if cursor != "" {
			v.Set("cursor", cursor)
		}

		endpoint := "https://slack.com/api/users.list?" + v.Encode()
		verboseLogf("Requesting users.list page %d (cursor present: %t)", page+1, cursor != "")

		// Issue the request with retry/backoff to respect rate limits
		var resp *http.Response
		for attempt := 0; ; attempt++ {
			verboseLogf("users.list page %d attempt %d", page+1, attempt+1)
			req, err := http.NewRequest("GET", endpoint, nil)
			if err != nil {
				return nil, fmt.Errorf("Got error %s when building the request", err.Error())
			}
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err = client.Do(req)
			if err != nil {
				// transient network error: basic exponential backoff up to ~8s
				d := time.Duration(1<<min(attempt, 3)) * time.Second
				verboseLogf("network error on users.list page %d: %v; retrying in %s", page+1, err, d)
				time.Sleep(d)
				continue
			}
			if resp.StatusCode == 429 {
				// Respect Slack rate limits: wait per Retry-After
				ra := resp.Header.Get("Retry-After")
				resp.Body.Close()
				waitSec := 1
				if ra != "" {
					if v, err := strconv.Atoi(ra); err == nil {
						waitSec = v
					}
				}
				verboseLogf("rate limited on users.list page %d; waiting %ds", page+1, waitSec)
				time.Sleep(time.Duration(waitSec) * time.Second)
				continue
			}
			if resp.StatusCode >= 500 {
				// server error: backoff and retry
				resp.Body.Close()
				d := time.Duration(1<<min(attempt, 3)) * time.Second
				verboseLogf("server error %d on users.list page %d; retrying in %s", resp.StatusCode, page+1, d)
				time.Sleep(d)
				continue
			}
			break
		}

		if resp.StatusCode != http.StatusOK {
			defer resp.Body.Close()
			return nil, fmt.Errorf("Slack API returned HTTP code %d", resp.StatusCode)
		}

		// Here SlackUser struct is used instead of interface{}.
		// It has very few fields defined, but the decoder will simply
		// ignore extra fields, and we only need a couple of them.
		var data struct {
			Ok      bool        `json:"ok"`
			Members []SlackUser `json:"members"`
			ResponseMetadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}

		err := json.NewDecoder(resp.Body).Decode(&data)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}

		if !data.Ok {
			return nil, errors.New("Unexpected lack of ok=true in Slack API response. Is access token correct?")
		}

		for _, user := range data.Members {
			if user.Id != "" && user.Profile.Email != "" {
				res[user.Id] = user.Profile.Email
			}
		}

		pageCount := len(data.Members)
		total += pageCount
		verboseLogf("users.list page %d fetched %d users (total so far: %d)", page+1, pageCount, total)

		if data.ResponseMetadata.NextCursor == "" {
			break
		}
		cursor = data.ResponseMetadata.NextCursor
		page++

		// Optional pacing between successive pages
		if usersPace1RPS {
			verboseLogf("pacing: sleeping 1s between page requests to respect ~1 rps guidance")
			time.Sleep(1 * time.Second)
		}
	}

	verboseLogf("Completed fetching users; total unique emails: %d", len(res))
	verbosePrintln("Fetched emails from Slack API. Now building a map of them to process.")

	return res, nil
}

// min returns the smaller of a, b.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
