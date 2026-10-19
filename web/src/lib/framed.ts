// A page inside somebody else's frame can be dressed up so that a click lands
// on Approve. The session cookie is not sent there anyway; this is the second lock.
export function isFramed(): boolean {
  try {
    return window.top !== window.self;
  } catch {
    return true;
  }
}
