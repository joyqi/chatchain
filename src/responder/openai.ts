import { Responder, Respond } from ".";
import { llmKey } from "../config";
import { OpenAI } from "langchain/llms/openai";

export default class extends Responder {
    respond(streaming: boolean): Respond {
        let isRephrasing = false;
        let cb: (str: string) => void;

        const model = new OpenAI({
            openAIApiKey: llmKey,
            streaming,
            callbacks: [
                {
                    handleLLMStart(_, prompts) {
                        isRephrasing = prompts[0].indexOf("Given") === 0;
                    },
                    handleLLMNewToken(token) {
                        if (!isRephrasing && cb) {
                            cb(token);
                        }
                    },
                }
            ]
        });

        const chain = this.createLLMChain(model);

        return async (question: string, fn?: (str: string) => void) => {
            if (streaming && fn) {
                cb = fn;
            }

            const { text } = await chain.call({ question, chat_history: this.history });

            if (!streaming && fn) {
                fn(text);
            }

            this.history.push(question, text);
            return text;
        }
    }
}