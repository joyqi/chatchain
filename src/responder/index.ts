import { BaseLLM } from "langchain/dist/llms/base";
import { llmLang } from "../config";
import { Store } from "../store";
import { ConversationalRetrievalQAChain, LLMChain, BaseChain } from "langchain/chains";
import { BasePromptTemplate, PromptTemplate } from "langchain/prompts";
import { ChainValues } from "langchain/schema";

const defaultQaTemplate = `Use the following pieces of context to answer the question ${llmLang ? `in ${llmLang}` : ''} at the end. If you don't know the answer, just say that you don't know, don't try to make up an answer.
{context}
Question: {question}
Helpful Answer:`;

export type LLMChainType = 'qa' | 'chat' | 'prompt';

export type Respond = (question: string | ChainValues, fn?: (str: string) => void) => Promise<string>;

export abstract class Responder {
    protected history: string[] = [];

    constructor(
        protected store: Store
    ) {}

    protected createLLMChain(model: BaseLLM, type?: LLMChainType, template?: string | BasePromptTemplate) {
        switch (type) {
            case 'chat':
                if (typeof template !== 'object') {
                    throw new Error("Please provide a BasePromptTemplate for a chat chain");
                }

                return new LLMChain({ llm: model, prompt: template });
            case 'prompt':
                const prompt = typeof template === 'string' || typeof template === 'undefined' ? new PromptTemplate({
                    template: template || `Q: {question}\nA:`,
                    inputVariables: ['question'] 
                }) : template;

                return new LLMChain({ llm: model, prompt });
            case 'qa':
            default:
                if (typeof template === 'object') {
                    throw new Error("Cannot use a BasePromptTemplate for a QA chain");
                }

                return ConversationalRetrievalQAChain.fromLLM(model, this.store.getVectorStore(), { qaTemplate: template || defaultQaTemplate });
        }
    }

    protected async callChain(chain: BaseChain, question: string | ChainValues) {
        if (typeof question === 'string') {
            const { text } = await chain.call({ question, chat_history: this.history });
            this.history.push(question, text);
            return text;
        } else {
            const { text } = await chain.call(question);
            return text;
        }
    }

    abstract respond(streaming: boolean, type?: LLMChainType, template?: string | BasePromptTemplate): Respond;
}

import OpenAIResponder from "./openai";

export { OpenAIResponder };