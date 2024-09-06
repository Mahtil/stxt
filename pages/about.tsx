import { css } from "@emotion/react";
import { colors } from "../util/theme";
import chroma from "chroma-js";

export default function ViewNote() {
  return (
    <div
      css={css`
        display: flex;
        flex-direction: column;

        h2 {
          color: ${chroma(colors.h1).mix("white", 0.2).css()};
          margin-block-start: 0px;
          margin-block-end: 0px;
        }
      `}
    >
      <h2>What is s.cr?</h2>
      <p className="faq-answer">
        s.cr encrypts your note in your browser, so only you and the recipient
        know its content. All notes self-destruct after a period of time,
        defaulting to 24 hours.
      </p>
      <h2>How is this secure?</h2>
      <p className="faq-answer">
        s.cr stores the encryption key within your URL fragment{" "}
        {"(the part after the #)"}. This key is never sent to the server, so the
        site operators have no way of decrypting the content. The fragment-key
        has 60 bits of entropy and we expand it through 128 rounds of pbkdf2
        before passing into AES-256. <br /> <br />
        But, you can only trust this site as much as you trust these claims.
      </p>
      <h2>How does this compare to privnote?</h2>
      <p className="faq-answer">
        The basic functionality is the same, but s.cr has no ads or trackers,
        and the URLs are shorter.
      </p>
      <h2>What is it used for?</h2>
      <p className="faq-answer">
        While s.cr could be used for any number of reasons, I use it for sharing
        passwords, secret keys, and messages that I may not want to be
        associated with in the future.
      </p>
      <h2>Who are you?</h2>
      <p className="faq-answer">
        I'm <a href="https://ammar.io">Ammar</a>. You can reach me{" "}
        <a href="mailto:a@s.cr">here</a>.
      </p>
    </div>
  );
}
