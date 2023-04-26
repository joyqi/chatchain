import { BaseLLM } from "langchain/dist/llms/base";
import { llmLang } from "../config";
import { Store } from "../store";
import { ConversationalRetrievalQAChain, LLMChain } from "langchain/chains";
import { PromptTemplate } from "langchain";

const defaultQaTemplate = `Use the following pieces of context to answer the question ${llmLang ? `in ${llmLang}` : ''} at the end. If you don't know the answer, just say that you don't know, don't try to make up an answer.
{context}
Question: {question}
Helpful Answer:`;

export type LLMChainType = 'qa' | 'chat';

export type Respond = (question: string, fn?: (str: string) => void) => Promise<string>;

export abstract class Responder {
    protected history: string[] = [];

    constructor(
        protected store: Store
    ) {}

    protected createLLMChain(model: BaseLLM, type?: LLMChainType, template?: string) {
        switch (type) {
            case 'chat':
                const prompt = new PromptTemplate({
                    template: template || `Q: {question}\nA:`,
                    inputVariables: ['question'] 
                });
                return new LLMChain({ llm: model, prompt });
            case 'qa':
            default:
                return ConversationalRetrievalQAChain.fromLLM(model, this.store.getVectorStore(), { qaTemplate: template || defaultQaTemplate });
        }
    }

    abstract respond(streaming: boolean, type?: LLMChainType, template?: string): Respond;
}

import OpenAIResponder from "./openai";

export { OpenAIResponder };