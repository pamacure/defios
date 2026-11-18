// Mentions as internal/mention finds them (ADR 0016), for the composer to
// mark while typing; docs/mentions/vectors.json is the contract. A message
// already sent comes from the shell split at its mentions.

const WORD = /[\p{L}\p{M}\p{N}]/u;
const ALNUM = /[A-Za-z0-9]/;

// mentionSpans is every mention in text, in order: text.slice(start, end)
// is its "@" and Handle as written, handle the Handle in lowercase.
export function mentionSpans(text) {
  const out = [];
  const chars = Array.from(text); // by code point, as Go reads runes
  let prev = ""; // what precedes chars[i]; "" at the start
  let at = 0; // the UTF-16 offset of chars[i]
  for (let i = 0; i < chars.length;) {
    const c = chars[i];
    if (c === "@" && prev !== "@" && prev !== "\\" && !WORD.test(prev)) {
      const end = match(chars, i + 1);
      if (end > i + 1) {
        const handle = chars.slice(i + 1, end).join("");
        if (!WORD.test(chars[end] ?? "")) out.push({ start: at, end: at + 1 + handle.length, handle: handle.toLowerCase() });
        prev = chars[end - 1];
        at += 1 + handle.length;
        i = end;
        continue;
      }
    }
    prev = c;
    at += c.length;
    i++;
  }
  return out;
}

// match is where the longest [A-Za-z0-9]+([-_][A-Za-z0-9]+)* from chars[i]
// ends; i for none.
function match(chars, i) {
  let end = i;
  while (end < chars.length && ALNUM.test(chars[end])) end++;
  if (end === i) return i;
  while (end + 1 < chars.length && (chars[end] === "-" || chars[end] === "_") && ALNUM.test(chars[end + 1])) {
    end += 2;
    while (end < chars.length && ALNUM.test(chars[end])) end++;
  }
  return end;
}
