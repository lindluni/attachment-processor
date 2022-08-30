# Jira Attachment Migrator

Download and install the [Jira Attachment Migrator](https://github.com/lindluni/jira-attachment-migrator/releases/tag/1.0.0)

## Build the Database

`jira-attachment-migrator collect --archive <path-to-archive> --github-token <github-token> --org <github-org> --repo <github-repo> --jira-username <jira-username> --jira-secret <jira-password-or-token> --jira-keys <jira-project-key-1,jira-project-key-2> --jira-url <jira-url>`

## Migrate the Attachments

`jira-attachment-migrator upload --jira-username <jira-username> --jira-secret <jira-password-or-token> --jira-url <jira-url>`

## Build the Process Attachment Archive

`jira-attachment-migrator archive`
