// Appended to the real Prose.tsx and linted under its path: Prose's exemption
// covers its one dangerouslySetInnerHTML and nothing else.
declare const el: HTMLElement;
declare const html: string;
declare const props: Record<string, unknown>;
export const s = <iframe srcdoc={html} />; // BAD
export const t = <div {...props} />; // BAD
el.insertAdjacentHTML("beforeend", html); // BAD
el.innerHTML = html; // BAD
export const u = <div dangerouslySetInnerHTML={{ __html: html }} />;
