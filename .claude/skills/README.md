# Claude Skills

AI-powered skills for the bootc-operator project. These are reusable instructions that AI agents (Claude Code, Copilot, etc.) can invoke to perform common development tasks — so you don't have to remember every command or worry about missing a step.

Each skill lives in its own directory as a `SKILL.md` file. When invoked, the agent follows the instructions step by step, checking prerequisites, running commands, and verifying results on your behalf.

## Available Skills

| Skill | Description |
|-------|-------------|
| [deploy-cluster](deploy-cluster/SKILL.md) | Build and deploy bootc-operator to a local bink cluster |
| [destroy-cluster](destroy-cluster/SKILL.md) | Tear down a local bink cluster and clean up resources |
| [k8s-api-review](k8s-api-review/SKILL.md) | Review Kubernetes API changes for convention compliance |
