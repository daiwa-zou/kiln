// Prose is the one place HTML enters the DOM as markup. Its only caller-side
// input is the output of the renderMarkdown port, which escapes the whole
// source before adding any tag of its own (the same contract as the legacy
// renderMarkdown, pinned by the characterization fixtures). The eslint config
// bans dangerouslySetInnerHTML and every other HTML sink everywhere else, and
// exempts this file alone, so a new sink cannot appear without editing it.
export function Prose({ html, class: className }: { html: string; class?: string }) {
  return <div class={className} dangerouslySetInnerHTML={{ __html: html }} />;
}
