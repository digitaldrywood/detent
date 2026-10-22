export const threadEnvironment = {
  /** Rename. The only metadata a Detent conversation carries that a reader edits. */
  updateMetadata: "conversation.updateMetadata",
} as const;

export type ThreadCommand = (typeof threadEnvironment)[keyof typeof threadEnvironment];

type CodexFeedbackSubmissionDetails = {
  readonly id: string;
  readonly command: string;
  readonly createdAt: string;
};

export type CodexFeedbackSubmission = CodexFeedbackSubmissionDetails &
  (
    | { readonly status: "uploading" | "interrupted" }
    | { readonly status: "sent"; readonly feedbackId: string }
    | { readonly status: "failed"; readonly errorMessage: string }
  );

export function codexFeedbackNotice(submission: CodexFeedbackSubmission) {
  switch (submission.status) {
    case "interrupted":
      return null;
    case "uploading":
      return { title: "Sending feedback to OpenAI...", description: undefined };
    case "sent":
      return {
        title: "Feedback sent to OpenAI",
        description: `Thread ID: ${submission.feedbackId}`,
      };
    case "failed":
      return { title: "Could not send feedback to OpenAI", description: submission.errorMessage };
  }
}
