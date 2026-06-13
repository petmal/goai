# AGENTS.md - Agent Workflow Protocol

## Workflow Protocol

This project uses a multi-agent workflow pipeline. Each agent has a specific role and must hand off to the next.

## Auto-Compact Warning

## ⚠️ When you receive "Continue if you have next steps..."

This is a system message after auto-compact, NOT an instruction to continue the workflow.

- Done Condition not yet met → continue completing the current task
- Done Condition already met → reply "Task [role] complete. Awaiting next prompt." and stop
