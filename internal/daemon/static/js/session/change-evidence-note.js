// The Sessions landing's coverage fact for change evidence
// (memory-store-unavailable-reads plan §2.6). The rail answer carries
// change_evidence_problem when the store could not be read, so the sessions
// known only from recorded change evidence are missing from the list; this is
// the sentence that says so. Pure (no DOM), so the words are tested; the
// caller renders it as text, because the reason names a store path.

function changeEvidenceNote(problem) {
  const reason = String(problem ?? '').trim();
  if (!reason) return '';
  return 'Sessions known only from recorded change evidence are not listed: ' + reason;
}

export { changeEvidenceNote };
