// Runs semantic-release, installed from package-lock.json in this directory,
// on the repository in the current directory (release.config.mjs), and
// passes what it released to the next jobs as step outputs.
import { appendFileSync } from "node:fs";
import semanticRelease from "semantic-release";

const result = await semanticRelease();

const outputs = { new_release_published: "false" };
if (result) {
  outputs.new_release_published = "true";
  outputs.new_release_version = result.nextRelease.version;
}

const lines = Object.entries(outputs).map(([key, value]) => `${key}=${value}\n`);
if (process.env.GITHUB_OUTPUT) {
  appendFileSync(process.env.GITHUB_OUTPUT, lines.join(""));
} else {
  process.stdout.write(lines.join(""));
}
