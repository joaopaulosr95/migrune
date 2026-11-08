// semantic-release configuration. A JS file rather than .releaserc.json so
// the release notes can drop commit trailers (see finalizeContext).
export default {
  branches: ["main"],
  tagFormat: "v${version}",
  plugins: [
    [
      "@semantic-release/commit-analyzer",
      {
        preset: "conventionalcommits",
      },
    ],
    [
      "@semantic-release/release-notes-generator",
      {
        preset: "conventionalcommits",
        writerOpts: {
          // A BREAKING CHANGE note runs to the end of the commit message, so
          // it would carry the Co-Authored-By trailer into the release notes.
          finalizeContext(context) {
            for (const group of context.noteGroups ?? []) {
              for (const note of group.notes) {
                note.text = note.text.replace(/^co-authored-by:.*$/gim, "").trim();
              }
            }
            return context;
          },
        },
      },
    ],
    [
      "@semantic-release/github",
      { successComment: false, failComment: false, releasedLabels: false },
    ],
  ],
};
