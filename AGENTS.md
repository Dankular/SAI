# Repository Guidelines

## Project Structure & Module Organization

This project is intended to provide a self-contained app for running A1’s agent loop and managing its lifecycle, using a local 4B model through OpenClaw. The current directory has no implementation files or Git metadata yet, so module locations are not established. As the code is added, keep application code under `src/`, tests under `tests/`, documentation under `docs/`, and shared assets under `assets/`. Keep model and runtime integration boundaries explicit.

## Build, Test, and Development Commands

No build system or runnable commands are configured yet. When introducing the app toolchain, document the actual setup, development, build, and test commands here. Keep local model and OpenClaw prerequisites clear, and make the development loop runnable without undocumented local-only steps.

## Coding Style & Naming Conventions

Follow the conventions of the language and framework selected for the app. Add formatter and linter configuration with the first implementation and apply it consistently. Use descriptive names, keep modules focused, and make agent-loop state transitions and lifecycle ownership clear in code.

## Testing Guidelines

There is no test framework or coverage target yet. Add tests with the first functional code, follow the selected framework’s naming conventions, and cover agent-loop transitions, lifecycle cleanup, and model/runtime error handling. Document the test command contributors should run.

## Commit & Pull Request Guidelines

There is no Git history from which to infer commit conventions. Use short, imperative commit subjects, such as `Add agent lifecycle handler`. Pull requests should explain the change and its motivation, list validation commands and results, link related issues, and include screenshots for user-visible interface changes.

## Security & Configuration

Do not commit credentials, private keys, or personal data. Keep local secrets in ignored environment files and provide sanitized examples for required configuration. Treat local model files and runtime endpoints as configurable environment-specific inputs.
