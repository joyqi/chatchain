import { Responder, Respond, LLMChainType } from ".";
import { llmKey } from "../config";
import { OpenAI, OpenAIChat } from "langchain/llms/openai";
import { ChatOpenAI } from "langchain/chat_models/openai";

type OpenAIOptions = ConstructorParameters<typeof OpenAI>[0] & ConstructorParameters<typeof ChatOpenAI>[0];

export default class extends Responder {
    respond(streaming: boolean, type?: LLMChainType, template?: string): Respond {
        let isRephrasing = false;
        let cb: (str: string) => void;
        const options: OpenAIOptions = {
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
        };

        const model = type === 'chat' ? new OpenAIChat(options) : new OpenAI(options);
        const chain = this.createLLMChain(model, type, template);

        return async (question, fn?: (str: string) => void) => {
            if (streaming && fn) {
                cb = fn;
            }

            const text = await this.callChain(chain, question);

            if (!streaming && fn) {
                fn(text);
            }

            return text;
        }
    }
}